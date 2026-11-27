package peer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Nodes of the test cluster: two voting nodes and a worker.
const (
	leaderHost   = "ilm.example.com"
	followerHost = "ilm2.example.com"
	workerHost   = "worker.example.com"
	followerIP   = "10.0.1.12"
	// Kubernetes registers the nodes under their short host names.
	leaderNode   = "ilm"
	followerNode = "ilm2"
	workerIP     = "10.0.1.20"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testCA is a CA that issues peer certificates in tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns the PEM certificate and key of a leaf with the common name cn for the addresses ips.
func (ca *testCA) issue(t *testing.T, cn string, ips ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// secret returns the peer Secret of the Kubernetes node node as cert-manager writes it: a leaf of ca for ips with the
// root CA.
func (ca *testCA) secret(t *testing.T, node, cn string, ips ...string) *corev1.Secret {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, cn, ips...)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: SecretName(node)},
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM, caKey: ca.pem},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// clusterAppliance returns the Appliance of a cluster of a leader, a follower and a worker.
func clusterAppliance() *v1alpha1.Appliance {
	return &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec: v1alpha1.ApplianceSpec{Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{
			{HostName: leaderHost, Address: "10.0.1.11/24", Role: v1alpha1.NodeRoleVoting},
			{HostName: followerHost, Address: followerIP + "/24", Role: v1alpha1.NodeRoleVoting},
			{HostName: workerHost, Address: workerIP + "/24", Role: v1alpha1.NodeRoleWorker},
		}}},
	}
}

// kubeNodes returns the Kubernetes nodes of the cluster, named by their short host names.
func kubeNodes() []client.Object {
	node := func(name, ip string) client.Object {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}},
		}
	}
	return []client.Object{node(leaderNode, "10.0.1.11"), node(followerNode, followerIP), node("worker", workerIP)}
}

// lease returns the leader election Lease held by holder.
func lease(holder string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: LeaseName},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}
}

func fakeReader(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

// identity returns the peer identity of the Kubernetes node node loaded from secret.
func identity(t *testing.T, node string, secret *corev1.Secret) *Identity {
	t.Helper()
	id := NewIdentity(quiet, fakeReader(t, secret), node)
	if err := id.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return id
}

// recorded is what the leader's API handler saw of a request.
type recorded struct {
	method, path, remote, header, cookie string
	body                                 []byte
}

// recordingAPI answers 201 with a cookie and records every request in seen.
func recordingAPI(seen chan<- recorded) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cookie := ""
		if c, err := r.Cookie("session"); err == nil {
			cookie = c.Value
		}
		seen <- recorded{
			method: r.Method, path: r.URL.Path, remote: r.RemoteAddr, header: r.Header.Get(ClientAddressHeader), cookie: cookie, body: body,
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "new"})
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// leaderServer starts the leader's peer listener on loopback, serving api to the voting nodes and the loopback address
// the tests connect from.
func leaderServer(t *testing.T, ca *testCA, api http.Handler, leading func() bool) (*httptest.Server, int) {
	t.Helper()
	return serveLeader(t, ca, api, leading, "10.0.1.11", followerIP, "127.0.0.1")
}

// serveLeader starts the peer listener of the leader on loopback, serving api to the voting nodes at voters.
func serveLeader(t *testing.T, ca *testCA, api http.Handler, leading func() bool, voters ...string) (*httptest.Server, int) {
	t.Helper()
	addrs := make([]netip.Addr, 0, len(voters))
	for _, v := range voters {
		addrs = append(addrs, mustAddr(v))
	}
	id := identity(t, leaderNode, ca.secret(t, leaderNode, CommonName, "127.0.0.1"))
	srv := httptest.NewUnstartedServer(Accept(api, leading))
	srv.TLS = id.ServerConfig(func(context.Context) ([]netip.Addr, error) { return addrs, nil })
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return srv, p
}

// forwarder returns the forwarder of id to the leader's peer listener on port.
func forwarder(id *Identity, port int) *Forwarder {
	f := NewForwarder(quiet, id)
	f.port = port
	return f
}

func always() bool { return true }
func never() bool  { return false }

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
