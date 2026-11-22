// Package access issues kubeconfigs for the cluster and talosconfigs for the node to the administrator, and keeps
// track of them without keeping any token, certificate or key.
package access

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Namespace holds the ServiceAccounts of the issued kubeconfigs and the record of the issued talosconfigs.
const Namespace = "appliance-access"

// The roles a kubeconfig can have.
const (
	RoleView         = "view"
	RoleClusterAdmin = "cluster-admin"
)

// ViewClusterRole is the ClusterRole of RoleView: what the view ClusterRole grants and read access to nodes, which
// view leaves out.
const ViewClusterRole = "appliance-access-view"

// LabelKind marks the ServiceAccounts and ClusterRoleBindings of issued kubeconfigs.
const LabelKind = "appliance.otilm.com/access"

// Annotations of a kubeconfig's ServiceAccount; AnnotationExpires is absent on a long-lived one.
const (
	AnnotationRole     = "appliance.otilm.com/role"
	AnnotationExpires  = "appliance.otilm.com/expires"
	AnnotationIssuedBy = "appliance.otilm.com/issued-by"
	AnnotationIssuedAt = "appliance.otilm.com/issued-at"
)

// MaxKubeconfigTTL is the longest a time-limited kubeconfig is valid.
const MaxKubeconfigTTL = 24 * time.Hour

const (
	generatedPrefix = "kc-"
	namedPrefix     = "named-"
	clusterName     = "ilm-appliance"
)

// Fields of a request that InvalidError names.
const (
	FieldRole  = "role"
	FieldHours = "hours"
	FieldName  = "name"
)

var (
	// ErrExists is returned for a named kubeconfig whose name is taken.
	ErrExists = errors.New("a kubeconfig with this name exists")
	// ErrNotFound is returned when revoking a kubeconfig that was not issued or is gone.
	ErrNotFound = errors.New("no such kubeconfig")

	validName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

	// tokenWait bounds the wait for the token controller to fill a long-lived token Secret, well inside the console's
	// write timeout; tokenPoll is how often it is checked.
	tokenWait = 15 * time.Second
	tokenPoll = 250 * time.Millisecond

	// cleanupWait bounds the revoke of a kubeconfig whose issue failed.
	cleanupWait = 10 * time.Second
)

// InvalidError is a request that is refused; Field names the request field at fault.
type InvalidError struct {
	Field, Message string
}

func (e *InvalidError) Error() string { return e.Field + ": " + e.Message }

// KubeRequest describes a kubeconfig to issue.
type KubeRequest struct {
	// Role is RoleView or RoleClusterAdmin.
	Role string
	// TTL is how long the token is valid, a whole number of hours up to MaxKubeconfigTTL; 0 is long-lived.
	TTL time.Duration
	// Name is required when TTL is 0.
	Name     string
	IssuedBy string
}

func (r KubeRequest) validate() error {
	if r.Role != RoleView && r.Role != RoleClusterAdmin {
		return &InvalidError{FieldRole, "Choose view or cluster-admin."}
	}
	if r.TTL != 0 && (r.TTL < time.Hour || r.TTL > MaxKubeconfigTTL || r.TTL%time.Hour != 0) {
		return &InvalidError{FieldHours, "Choose 1 to 24 hours."}
	}
	if r.TTL == 0 && r.Name == "" {
		return &InvalidError{FieldName, "A long-lived kubeconfig needs a name."}
	}
	if r.Name != "" && !validName.MatchString(r.Name) {
		return &InvalidError{FieldName, "Use up to 40 lower-case letters, digits and hyphens, starting and ending with a letter or digit."}
	}
	if r.Name == BreakGlass {
		return &InvalidError{FieldName, "The name break-glass is reserved for the recovery kit."}
	}
	return nil
}

// BreakGlass is the ServiceAccount of the recovery kit's kubeconfig. Issuing the kit again replaces it; it is not
// revoked on its own and never expires.
const BreakGlass = "break-glass"

// IssueBreakGlass returns a long-lived cluster-admin kubeconfig for server, trusting caPEM, as BreakGlass. An
// earlier break-glass ServiceAccount is deleted first, so its token stops working.
func IssueBreakGlass(ctx context.Context, c client.Client, cs kubernetes.Interface, server string, caPEM []byte, issuedBy string) ([]byte, error) {
	if err := revokeBreakGlass(ctx, c); err != nil {
		return nil, err
	}
	data, _, err := issue(ctx, c, cs, server, caPEM, BreakGlass, KubeRequest{Role: RoleClusterAdmin, IssuedBy: issuedBy})
	return data, err
}

// revokeBreakGlass deletes the break-glass binding and ServiceAccount; the garbage collector deletes its token Secret.
func revokeBreakGlass(ctx context.Context, c client.Client) error {
	for _, obj := range []client.Object{
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName(BreakGlass)}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: BreakGlass}},
	} {
		if err := c.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete the break-glass %T: %w", obj, err)
		}
	}
	return nil
}

// IssueKubeconfig creates a ServiceAccount bound to the request's role and returns a kubeconfig for server, trusting
// caPEM, and the ServiceAccount's name, which revokes it. The token expires after the TTL; with TTL 0 it is long-lived.
func IssueKubeconfig(ctx context.Context, c client.Client, cs kubernetes.Interface, server string, caPEM []byte, req KubeRequest) ([]byte, string, error) {
	if err := req.validate(); err != nil {
		return nil, "", err
	}
	name := ""
	if req.Name != "" {
		name = namedPrefix + req.Name
	}
	return issue(ctx, c, cs, server, caPEM, name, req)
}

// issue is IssueKubeconfig for the ServiceAccount name, generated when empty, without validating req.
func issue(ctx context.Context, c client.Client, cs kubernetes.Interface, server string, caPEM []byte, name string, req KubeRequest) ([]byte, string, error) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace:   Namespace,
		Name:        name,
		Labels:      map[string]string{LabelKind: KindKubeconfig},
		Annotations: map[string]string{AnnotationRole: req.Role, AnnotationIssuedBy: req.IssuedBy},
	}}
	if name == "" {
		sa.GenerateName = generatedPrefix
	}
	issued := now().UTC()
	sa.Annotations[AnnotationIssuedAt] = issued.Format(time.RFC3339)
	if req.TTL > 0 {
		sa.Annotations[AnnotationExpires] = issued.Add(req.TTL).Format(time.RFC3339)
	}
	if err := c.Create(ctx, sa, client.FieldOwner(kube.ManagerFieldManager)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, "", ErrExists
		}
		return nil, "", fmt.Errorf("create ServiceAccount %s/%s: %w", Namespace, name, err)
	}
	token, err := bindAndToken(ctx, c, cs, sa, req)
	if err != nil {
		// The cleanup outlives the request: a client that went away must not leave a bound ServiceAccount behind.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
		defer cancel()
		if rerr := Revoke(cleanup, c, sa.Name); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			err = errors.Join(err, rerr)
		}
		return nil, "", err
	}
	data, err := kubeconfig(server, caPEM, sa.Name, token)
	if err != nil {
		return nil, "", err
	}
	return data, sa.Name, nil
}

// bindAndToken binds sa to the request's role and returns its token.
func bindAndToken(ctx context.Context, c client.Client, cs kubernetes.Interface, sa *corev1.ServiceAccount, req KubeRequest) (string, error) {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName(sa.Name), Labels: map[string]string{LabelKind: KindKubeconfig}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole(req.Role)},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: Namespace, Name: sa.Name}},
	}
	if err := c.Create(ctx, crb, client.FieldOwner(kube.ManagerFieldManager)); err != nil {
		return "", fmt.Errorf("bind ServiceAccount %s/%s to %s: %w", Namespace, sa.Name, req.Role, err)
	}
	if req.TTL > 0 {
		seconds := int64(req.TTL / time.Second)
		tr, err := cs.CoreV1().ServiceAccounts(Namespace).CreateToken(ctx, sa.Name,
			&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("request a token for ServiceAccount %s/%s: %w", Namespace, sa.Name, err)
		}
		return tr.Status.Token, nil
	}
	return longLivedToken(ctx, c, cs, sa)
}

// longLivedToken creates the service account token Secret of sa, owned by it, and waits for the token controller
// to write its token. The Secret's name is generated: the Secret of a revoked ServiceAccount of the same name may
// still wait for the garbage collector.
func longLivedToken(ctx context.Context, c client.Client, cs kubernetes.Interface, sa *corev1.ServiceAccount) (string, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       Namespace,
			GenerateName:    sa.Name + "-",
			Labels:          map[string]string{LabelKind: KindKubeconfig},
			Annotations:     map[string]string{corev1.ServiceAccountNameKey: sa.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ServiceAccount", Name: sa.Name, UID: sa.UID}},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
	if err := c.Create(ctx, secret, client.FieldOwner(kube.ManagerFieldManager)); err != nil {
		return "", fmt.Errorf("create the token Secret of ServiceAccount %s/%s: %w", Namespace, sa.Name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, tokenWait)
	defer cancel()
	ticker := time.NewTicker(tokenPoll)
	defer ticker.Stop()
	for {
		s, err := cs.CoreV1().Secrets(Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
		if err == nil && len(s.Data[corev1.ServiceAccountTokenKey]) > 0 {
			return string(s.Data[corev1.ServiceAccountTokenKey]), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for the token of Secret %s/%s: %w", Namespace, secret.Name, errors.Join(ctx.Err(), err))
		case <-ticker.C:
		}
	}
}

// kubeconfig returns a kubeconfig for server, trusting caPEM, that authenticates as user with token.
func kubeconfig(server string, caPEM []byte, user, token string) ([]byte, error) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterName] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: caPEM}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts[user] = &clientcmdapi.Context{Cluster: clusterName, AuthInfo: user}
	cfg.CurrentContext = user
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("encode the kubeconfig: %w", err)
	}
	return data, nil
}

func clusterRole(role string) string {
	if role == RoleView {
		return ViewClusterRole
	}
	return role
}

func bindingName(name string) string {
	return Namespace + ":" + name
}

// Revoke deletes the ClusterRoleBinding and the ServiceAccount of the kubeconfig name; the garbage collector deletes
// its token Secret. It returns ErrNotFound for a ServiceAccount that is not an issued kubeconfig's.
func Revoke(ctx context.Context, c client.Client, name string) error {
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, sa); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("read ServiceAccount %s/%s: %w", Namespace, name, err)
	}
	if sa.Labels[LabelKind] != KindKubeconfig {
		return ErrNotFound
	}
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName(name)}}
	if err := c.Delete(ctx, crb); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete ClusterRoleBinding %s: %w", crb.Name, err)
	}
	if err := c.Delete(ctx, sa, client.Preconditions{UID: &sa.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete ServiceAccount %s/%s: %w", Namespace, name, err)
	}
	return nil
}
