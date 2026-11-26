package ilmclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// IdentitySecret, in the system namespace, holds the manager's ILM certificate and key and, once registered, the
// UUID of its ILM user under UserUUIDKey. During a renewal it also holds the new certificate and key under
// PendingCertKey and PendingKeyKey until ILM has taken the new certificate.
const (
	IdentitySecret = "appliance-ilm-identity"
	UserUUIDKey    = "userUuid"
	PendingCertKey = "pending.crt"
	PendingKeyKey  = "pending.key"
)

const (
	// commonName is the subject of the manager's certificate; usernamePrefix starts its ILM user name, which ends
	// with the certificate's fingerprint, so that a new registration never meets a taken name: ILM answers that
	// with a server error.
	commonName     = "appliance-manager"
	usernamePrefix = "appliance-manager-"
	description    = "ILM Appliance Manager"
	// certificateLifetime is how long an issued certificate is valid; it is renewed renewBefore its expiry.
	certificateLifetime = 365 * 24 * time.Hour
	renewBefore         = 30 * 24 * time.Hour
	// clockSkew backdates a certificate's start.
	clockSkew = 5 * time.Minute
	// fingerprintChars of the fingerprint end the user name.
	fingerprintChars = 16
	// coreDeployment and corePort are Core's Deployment in the platform namespace and its HTTP port.
	coreDeployment = "core"
	corePort       = 8080
)

// Identity keeps the manager's ILM identity: a certificate of ILM's administrator CA, registered as an ILM
// superadmin through Core's bootstrap endpoint, which answers only to localhost, and renewed before it expires.
type Identity struct {
	// Client reads the administrator CA in the platform namespace and keeps the identity Secret; it reads past any
	// cache.
	Client client.Client
	// Forward makes Core reachable as localhost and returns its base URL and a function that ends the forward.
	Forward func(context.Context) (string, func(), error)
	Now     func() time.Time
	// Backups, when set, keeps a renewal apart from a backup, so that a backup never holds a certificate that its
	// database no longer accepts.
	Backups Exclusive

	// held keeps the callers of Ensure from registering or renewing at once; a caller waits for it only as long as
	// its context allows.
	held     chan struct{}
	heldOnce sync.Once
}

// Exclusive runs a function while no backup runs and refuses without running it while one does.
type Exclusive interface {
	Exclusive(run func() error) error
}

// identity is the content of the identity Secret.
type identity struct {
	cert tls.Certificate
	// pending is the certificate of a renewal that ILM may not have taken yet.
	pending  *tls.Certificate
	userUUID string
}

// lock takes the identity for one caller of Ensure, or fails with ctx's error when ctx ends first.
func (i *Identity) lock(ctx context.Context) error {
	i.heldOnce.Do(func() { i.held = make(chan struct{}, 1) })
	select {
	case i.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (i *Identity) unlock() { <-i.held }

// Ensure returns a client of the ILM edge at edge for ilmHost, trusting roots, authenticated with the manager's
// certificate. Whether to register a new user is decided from the stored certificate alone, never from an answer of
// ILM, so that an outage of ILM's authentication never creates users: a new certificate is issued and registered
// when there is none, or when the stored one expired or is not issued by the current administrator CA. A
// certificate that expires within renewBefore is renewed for the same user. A valid certificate that ILM refuses
// gives the client's authFailed on the calls.
func (i *Identity) Ensure(ctx context.Context, edge, ilmHost string, roots *x509.CertPool) (*Client, error) {
	if err := i.lock(ctx); err != nil {
		return nil, err
	}
	defer i.unlock()
	ca, err := i.adminCA(ctx)
	if err != nil {
		return nil, err
	}
	id, err := i.load(ctx)
	if err != nil {
		return nil, err
	}
	if id != nil && id.pending != nil {
		if id, err = i.settlePending(ctx, id, ca, edge, ilmHost, roots); err != nil {
			return nil, err
		}
	}
	if id, err = i.usableOrNew(ctx, id, ca); err != nil {
		return nil, err
	}
	c := New(edge, ilmHost, id.cert, roots)
	if id.userUUID == "" {
		if id.userUUID, err = i.register(ctx, c, id.cert); err != nil {
			return nil, err
		}
		if err := i.store(ctx, id); err != nil {
			return nil, err
		}
		log.FromContext(ctx).Info("registered the manager's certificate in ILM", "user", id.userUUID)
		return c, nil
	}
	return i.renew(ctx, c, id, ca, edge, ilmHost, roots)
}

// usableOrNew returns id when it can be used, otherwise a newly issued identity that is not registered yet.
func (i *Identity) usableOrNew(ctx context.Context, id *identity, ca tls.Certificate) (*identity, error) {
	why := i.unusable(id, ca)
	if why == "" {
		return id, nil
	}
	previous := ""
	if id != nil {
		previous = id.userUUID
	}
	log.FromContext(ctx).Info("issuing and registering a new ILM certificate", "why", why, "previousUser", previous)
	return i.issue(ctx, ca)
}

// unusable tells why the stored identity cannot be used, or "" when it can.
func (i *Identity) unusable(id *identity, ca tls.Certificate) string {
	if id == nil {
		return "no certificate is stored"
	}
	return i.invalid(id.cert.Leaf, ca.Leaf)
}

// invalid tells why cert cannot be used with the administrator CA ca, or "" when it can: it expired, or ca did not
// issue it. The issuer is checked by its name and signature only, not by the CA's own extensions, so that a CA
// certificate without the CA flags never makes every certificate look foreign.
func (i *Identity) invalid(cert, ca *x509.Certificate) string {
	switch {
	case !i.Now().Before(cert.NotAfter):
		return "the certificate expired"
	case !bytes.Equal(cert.RawIssuer, ca.RawSubject) ||
		ca.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) != nil:
		return "the certificate is not issued by the current administrator CA"
	}
	return ""
}

// renew replaces the certificate of the user when it expires within renewBefore and returns the client to use. While
// a backup runs it keeps the stored certificate.
func (i *Identity) renew(ctx context.Context, c *Client, id *identity, ca tls.Certificate, edge, ilmHost string, roots *x509.CertPool) (*Client, error) {
	if i.Now().Before(id.cert.Leaf.NotAfter.Add(-renewBefore)) {
		return c, nil
	}
	if i.Backups == nil {
		return i.replace(ctx, c, id, ca, edge, ilmHost, roots)
	}
	ran, renewed := false, c
	err := i.Backups.Exclusive(func() error {
		ran = true
		var err error
		renewed, err = i.replace(ctx, c, id, ca, edge, ilmHost, roots)
		return err
	})
	if !ran {
		// The stored certificate is still valid; a later pass renews it.
		// err is the fixed refusal of Exclusive: a backup runs or an update bundle loads.
		log.FromContext(ctx).Info("the manager's ILM certificate is renewed later", "refused", err.Error(), "user", id.userUUID)
		return c, nil
	}
	return renewed, err
}

// replace issues a new certificate for the user of id and has ILM take it. The new certificate is stored as pending
// before ILM is asked to take it, so that a crash at any point leaves both certificates for settlePending to choose
// from.
func (i *Identity) replace(ctx context.Context, c *Client, id *identity, ca tls.Certificate, edge, ilmHost string, roots *x509.CertPool) (*Client, error) {
	cert, err := i.newCertificate(ca)
	if err != nil {
		return nil, err
	}
	id.pending = &cert
	if err := i.store(ctx, id); err != nil {
		return nil, err
	}
	body := map[string]any{"description": description, "certificateData": base64.StdEncoding.EncodeToString(cert.Certificate[0])}
	if _, err := c.call(ctx, http.MethodPut, "/api/v1/users/"+url.PathEscape(id.userUUID), body); err != nil {
		return nil, err
	}
	renewed := &identity{cert: cert, userUUID: id.userUUID}
	if err := i.store(ctx, renewed); err != nil {
		return nil, err
	}
	log.FromContext(ctx).Info("renewed the manager's ILM certificate", "user", id.userUUID, "notAfter", cert.Leaf.NotAfter)
	return New(edge, ilmHost, cert, roots), nil
}

// settlePending finishes a renewal that a failure cut off, keeping a certificate ILM may accept:
//   - a pending certificate that expired or is not issued by the current CA is dropped;
//   - one ILM accepts replaces the stored one;
//   - one ILM refuses is dropped when ILM accepts the stored one, or when the stored one is unusable, which Ensure
//     then replaces by a new registration. When ILM refuses both, both are kept and the refusal is returned: ILM
//     may have taken the pending certificate while its authentication is down.
//
// Any other failure keeps both for the next attempt.
func (i *Identity) settlePending(ctx context.Context, id *identity, ca tls.Certificate, edge, ilmHost string, roots *x509.CertPool) (*identity, error) {
	current := &identity{cert: id.cert, userUUID: id.userUUID}
	if why := i.invalid(id.pending.Leaf, ca.Leaf); why != "" {
		log.FromContext(ctx).Info("dropping the pending ILM certificate", "why", why, "user", id.userUUID)
		return current, i.store(ctx, current)
	}
	err := New(edge, ilmHost, *id.pending, roots).Profile(ctx)
	switch {
	case err == nil:
		promoted := &identity{cert: *id.pending, userUUID: id.userUUID}
		log.FromContext(ctx).Info("ILM took the renewed certificate", "user", id.userUUID)
		return promoted, i.store(ctx, promoted)
	case Reason(err) != ReasonAuthFailed:
		return nil, err
	}
	if why := i.invalid(id.cert.Leaf, ca.Leaf); why != "" {
		// Neither certificate can be used: Ensure decides from the stored one alone.
		log.FromContext(ctx).Info("dropping the refused pending ILM certificate", "storedCertificate", why, "user", id.userUUID)
		return current, i.store(ctx, current)
	}
	if err := New(edge, ilmHost, id.cert, roots).Profile(ctx); err != nil {
		return nil, err
	}
	return current, i.store(ctx, current)
}

// register registers cert as a superadmin through Core's bootstrap endpoint and returns the user's UUID. A
// certificate already registered, by an earlier attempt that did not store the UUID, is looked up through the edge.
func (i *Identity) register(ctx context.Context, edge *Client, cert tls.Certificate) (string, error) {
	base, stop, err := i.Forward(ctx)
	if err != nil {
		return "", &Error{Op: "reach Core", Reason: ReasonUnreachable, cause: err}
	}
	defer stop()
	username := username(cert)
	body := map[string]any{
		"username": username, "description": description, "enabled": true,
		"certificateData": base64.StdEncoding.EncodeToString(cert.Certificate[0]),
	}
	answer, err := newLocal(base).call(ctx, http.MethodPost, "/api/v1/local/admins", body)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusUnprocessableEntity {
		return lookupUser(ctx, edge, username)
	}
	if err != nil {
		return "", err
	}
	var user struct {
		UUID string `json:"uuid"`
	}
	if err := decode("register the manager", answer, &user); err != nil {
		return "", err
	}
	if user.UUID == "" {
		return "", &Error{Op: "register the manager", Reason: ReasonFailed, cause: errors.New("the answer names no user")}
	}
	return user.UUID, nil
}

// lookupUser returns the UUID of the user username, asking as that user.
func lookupUser(ctx context.Context, c *Client, username string) (string, error) {
	const path = "/api/v1/users"
	answer, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	var users []struct {
		UUID     string `json:"uuid"`
		Username string `json:"username"`
	}
	if err := decode(http.MethodGet+" "+path, answer, &users); err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Username == username {
			return u.UUID, nil
		}
	}
	return "", &Error{Op: "find the manager's user", Reason: ReasonNotFound}
}

// fingerprint is the lower-case hex SHA-256 of a DER certificate, by which ILM finds a certificate's user.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func username(cert tls.Certificate) string {
	return usernamePrefix + fingerprint(cert.Certificate[0])[:fingerprintChars]
}

// load reads the identity Secret, or returns nil when there is none or it holds no usable certificate. A pending
// certificate that cannot be read is left out.
func (i *Identity) load(ctx context.Context) (*identity, error) {
	s := &corev1.Secret{}
	err := i.Client.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: IdentitySecret}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s: %w", kube.SystemNamespace, IdentitySecret, err)
	}
	cert, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		log.FromContext(ctx).Info("the identity Secret holds no usable certificate", "secret", IdentitySecret)
		return nil, nil
	}
	id := &identity{cert: cert, userUUID: string(s.Data[UserUUIDKey])}
	if pending, err := tls.X509KeyPair(s.Data[PendingCertKey], s.Data[PendingKeyKey]); err == nil {
		id.pending = &pending
	}
	return id, nil
}

// issue issues a certificate and stores it before it is registered, so that a registered certificate is never lost.
func (i *Identity) issue(ctx context.Context, ca tls.Certificate) (*identity, error) {
	cert, err := i.newCertificate(ca)
	if err != nil {
		return nil, err
	}
	id := &identity{cert: cert}
	if err := i.store(ctx, id); err != nil {
		return nil, err
	}
	return id, nil
}

func (i *Identity) adminCA(ctx context.Context) (tls.Certificate, error) {
	s := &corev1.Secret{}
	if err := i.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminCASecret}, s); err != nil {
		return tls.Certificate{}, fmt.Errorf("read secret %s/%s: %w", platform.Namespace, platform.AdminCASecret, err)
	}
	ca, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("secret %s/%s holds no usable key pair", platform.Namespace, platform.AdminCASecret)
	}
	return ca, nil
}

// newCertificate issues a client certificate of the CA with a new P-256 key.
func (i *Identity) newCertificate(ca tls.Certificate) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate a key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate a serial number: %w", err)
	}
	now := i.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(certificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Leaf, key.Public(), ca.PrivateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("issue the manager's certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read the manager's certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func pemPair(cert tls.Certificate) ([]byte, []byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the manager's key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func (i *Identity) store(ctx context.Context, id *identity) error {
	certPEM, keyPEM, err := pemPair(id.cert)
	if err != nil {
		return err
	}
	data := map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
	if id.userUUID != "" {
		data[UserUUIDKey] = []byte(id.userUUID)
	}
	if id.pending != nil {
		if data[PendingCertKey], data[PendingKeyKey], err = pemPair(*id.pending); err != nil {
			return err
		}
	}
	s := &corev1.Secret{}
	err = i.Client.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: IdentitySecret}, s)
	switch {
	case apierrors.IsNotFound(err):
		s = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: IdentitySecret},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		err = i.Client.Create(ctx, s, client.FieldOwner(kube.ManagerFieldManager))
	case err == nil:
		s.Data = data
		err = i.Client.Update(ctx, s, client.FieldOwner(kube.ManagerFieldManager))
	}
	if err != nil {
		return fmt.Errorf("write secret %s/%s: %w", kube.SystemNamespace, IdentitySecret, err)
	}
	return nil
}

// CoreForward returns a Forward that port-forwards a local port to Core's HTTP port in a ready pod of Core's
// Deployment, through the API server at cfg: Core takes the forwarded connection for localhost. The manager runs in
// the host network, so while the forward is open any process on the node can reach Core as localhost on its
// ephemeral port; the forward is open only for the registration call.
func CoreForward(cfg *rest.Config, c client.Reader) func(context.Context) (string, func(), error) {
	return func(ctx context.Context) (string, func(), error) {
		pod, err := corePod(ctx, c)
		if err != nil {
			return "", nil, err
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return "", nil, fmt.Errorf("port-forward client: %w", err)
		}
		transport, upgrader, err := spdy.RoundTripperFor(cfg)
		if err != nil {
			return "", nil, fmt.Errorf("port-forward transport: %w", err)
		}
		u := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(platform.Namespace).Name(pod).SubResource("portforward").URL()
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
		stop, ready := make(chan struct{}), make(chan struct{})
		pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(corePort)}, stop, ready, io.Discard, io.Discard)
		if err != nil {
			return "", nil, fmt.Errorf("port-forward to %s/%s: %w", platform.Namespace, pod, err)
		}
		failed := make(chan error, 1)
		go func() { failed <- pf.ForwardPorts() }()
		select {
		case <-ready:
		case err := <-failed:
			return "", nil, fmt.Errorf("port-forward to %s/%s: %w", platform.Namespace, pod, err)
		case <-ctx.Done():
			close(stop)
			return "", nil, ctx.Err()
		}
		ports, err := pf.GetPorts()
		if err == nil && len(ports) == 0 {
			err = errors.New("none is listed")
		}
		if err != nil {
			close(stop)
			return "", nil, fmt.Errorf("port-forward to %s/%s has no local port: %w", platform.Namespace, pod, err)
		}
		return "http://127.0.0.1:" + strconv.Itoa(int(ports[0].Local)), func() { close(stop) }, nil
	}
}

// corePod returns the name of a running, ready pod of Core's Deployment.
func corePod(ctx context.Context, c client.Reader) (string, error) {
	d := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: coreDeployment}, d); err != nil {
		return "", fmt.Errorf("read deployment %s/%s: %w", platform.Namespace, coreDeployment, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("deployment %s/%s: %w", platform.Namespace, coreDeployment, err)
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(platform.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return "", fmt.Errorf("list the pods of %s/%s: %w", platform.Namespace, coreDeployment, err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil && podReady(&p) {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("deployment %s/%s has no ready pod", platform.Namespace, coreDeployment)
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
