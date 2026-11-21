package access

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testServer = "https://192.0.2.10:6443"

var testCA = []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")

// issued parses a kubeconfig and returns its only user's token after checking the cluster it points at.
func issued(t *testing.T, data []byte) (*clientcmdapi.Config, string) {
	t.Helper()
	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Contexts[cfg.CurrentContext]
	if current == nil {
		t.Fatalf("no current context %q", cfg.CurrentContext)
	}
	cluster := cfg.Clusters[current.Cluster]
	if cluster.Server != testServer || string(cluster.CertificateAuthorityData) != string(testCA) {
		t.Errorf("cluster %s with CA %q", cluster.Server, cluster.CertificateAuthorityData)
	}
	user := cfg.AuthInfos[current.AuthInfo]
	if user == nil || user.Token == "" {
		t.Fatal("the kubeconfig has no token")
	}
	return cfg, user.Token
}

// account returns the only access ServiceAccount.
func account(t *testing.T) *corev1.ServiceAccount {
	t.Helper()
	list := &corev1.ServiceAccountList{}
	if err := testClient.List(context.Background(), list, client.InNamespace(Namespace), client.MatchingLabels{LabelKind: KindKubeconfig}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%d access ServiceAccounts, want 1", len(list.Items))
	}
	return &list.Items[0]
}

// tokenExpiry returns the expiry of a JWT without verifying it.
func tokenExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("the token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return time.Unix(claims.Exp, 0)
}

// as returns a clientset that authenticates with token.
func as(t *testing.T, token string) kubernetes.Interface {
	t.Helper()
	cfg := rest.AnonymousClientConfig(testConfig)
	cfg.BearerToken = token
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestIssueKubeconfigTimeLimited(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	before := time.Now()
	data, issuedName, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleView, TTL: 2 * time.Hour, IssuedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	_, token := issued(t, data)
	if exp := tokenExpiry(t, token); exp.Before(before.Add(2*time.Hour-time.Minute)) || exp.After(before.Add(2*time.Hour+time.Minute)) {
		t.Errorf("token expires at %v, want two hours after %v", exp, before)
	}

	sa := account(t)
	if issuedName != sa.Name {
		t.Errorf("issued %s, ServiceAccount %s", issuedName, sa.Name)
	}
	if !strings.HasPrefix(sa.Name, "kc-") {
		t.Errorf("ServiceAccount %s, want a generated kc- name", sa.Name)
	}
	if sa.Annotations[AnnotationRole] != RoleView || sa.Annotations[AnnotationIssuedBy] != "admin" || sa.Annotations[AnnotationIssuedAt] == "" {
		t.Errorf("annotations %v", sa.Annotations)
	}
	expires, err := time.Parse(time.RFC3339, sa.Annotations[AnnotationExpires])
	if err != nil || expires.Before(before.Add(2*time.Hour-time.Minute)) || expires.After(before.Add(2*time.Hour+time.Minute)) {
		t.Errorf("expires annotation %q", sa.Annotations[AnnotationExpires])
	}
	crb := &rbacv1.ClusterRoleBinding{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: bindingName(sa.Name)}, crb); err != nil {
		t.Fatal(err)
	}
	if crb.RoleRef.Name != ViewClusterRole || len(crb.Subjects) != 1 || crb.Subjects[0].Name != sa.Name || crb.Subjects[0].Namespace != Namespace {
		t.Errorf("binding %+v %+v", crb.RoleRef, crb.Subjects)
	}

	// The token authenticates as the ServiceAccount and is refused a change. envtest does not aggregate the view
	// role, so what it may read is checked on a real cluster only.
	viewer := as(t, token)
	review, err := viewer.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := review.Status.UserInfo.Username; got != "system:serviceaccount:"+Namespace+":"+sa.Name {
		t.Errorf("the token authenticates as %s", got)
	}
	err = viewer.CoreV1().Pods("default").Delete(ctx, "any", metav1.DeleteOptions{})
	if !apierrors.IsForbidden(err) {
		t.Errorf("delete a pod with the view token: %v, want forbidden", err)
	}
}

func TestIssueKubeconfigLongLivedNeedsName(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	_, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleClusterAdmin, IssuedBy: "admin"})
	var invalid *InvalidError
	if !errors.As(err, &invalid) || invalid.Field != FieldName {
		t.Fatalf("long-lived without a name: %v, want an invalid name", err)
	}

	fillTokens(t, "long-lived-token-value")
	data, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleClusterAdmin, Name: "ci", IssuedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, token := issued(t, data); token != "long-lived-token-value" {
		t.Error("the kubeconfig does not carry the Secret's token")
	}
	sa := account(t)
	if sa.Name != "named-ci" || sa.Annotations[AnnotationExpires] != "" || sa.Annotations[AnnotationRole] != RoleClusterAdmin {
		t.Errorf("ServiceAccount %s with annotations %v", sa.Name, sa.Annotations)
	}
	owned := ownedSecrets(t, sa)
	if len(owned) != 1 || owned[0].Type != corev1.SecretTypeServiceAccountToken || owned[0].Annotations[corev1.ServiceAccountNameKey] != sa.Name {
		t.Errorf("token Secrets %+v", owned)
	}

	if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleView, Name: "ci", IssuedBy: "admin"}); !errors.Is(err, ErrExists) {
		t.Errorf("the same name again: %v, want ErrExists", err)
	}
}

func TestIssueKubeconfigRefusesBadRequests(t *testing.T) {
	resetAccess(t)
	for _, tc := range []struct {
		req   KubeRequest
		field string
	}{
		{KubeRequest{Role: "edit", TTL: time.Hour}, FieldRole},
		{KubeRequest{Role: RoleView, TTL: 30 * time.Minute}, FieldHours},
		{KubeRequest{Role: RoleView, TTL: 25 * time.Hour}, FieldHours},
		{KubeRequest{Role: RoleView, Name: "Not_A_Name"}, FieldName},
		{KubeRequest{Role: RoleView, Name: strings.Repeat("a", 41)}, FieldName},
		{KubeRequest{Role: RoleClusterAdmin, Name: BreakGlass}, FieldName},
	} {
		_, _, err := IssueKubeconfig(context.Background(), testClient, testKube, testServer, testCA, tc.req)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Field != tc.field {
			t.Errorf("%+v: %v, want an invalid %s", tc.req, err, tc.field)
		}
	}
}

func TestIssueKubeconfigCleansUpWithoutAToken(t *testing.T) {
	resetAccess(t)
	defer func(wait time.Duration) { tokenWait = wait }(tokenWait)
	tokenWait = 100 * time.Millisecond
	_, _, err := IssueKubeconfig(context.Background(), testClient, testKube, testServer, testCA, KubeRequest{Role: RoleView, Name: "stuck", IssuedBy: "admin"})
	if err == nil {
		t.Fatal("expected an error while no token is written")
	}
	list := &corev1.ServiceAccountList{}
	if err := testClient.List(context.Background(), list, client.InNamespace(Namespace), client.MatchingLabels{LabelKind: KindKubeconfig}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d ServiceAccounts left behind", len(list.Items))
	}
}

func TestRevokeDeletesAccount(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleView, TTL: time.Hour, IssuedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	name := account(t).Name
	if err := Revoke(ctx, testClient, name); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("ServiceAccount after revoke: %v", err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Name: bindingName(name)}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("binding after revoke: %v", err)
	}
	if err := Revoke(ctx, testClient, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke twice: %v, want ErrNotFound", err)
	}

	// A ServiceAccount the appliance did not issue is not revoked.
	other := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: "other"}}
	if err := testClient.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := Revoke(ctx, testClient, "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke a foreign ServiceAccount: %v, want ErrNotFound", err)
	}
}

func TestReissueTheNameOfARevokedKubeconfig(t *testing.T) {
	resetAccess(t)
	fillTokens(t, "token")
	ctx := context.Background()
	req := KubeRequest{Role: RoleView, Name: "ci", IssuedBy: "admin"}
	_, name, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, req)
	if err != nil {
		t.Fatal(err)
	}
	// envtest runs no garbage collector: the token Secret of the revoked ServiceAccount stays, as it may for a while
	// in a cluster.
	if err := Revoke(ctx, testClient, name); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, req); err != nil {
		t.Fatalf("issue the name of a revoked kubeconfig again: %v", err)
	}
}

func TestExpiredAccountsRemoved(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	fillTokens(t, "kept-token")
	for _, req := range []KubeRequest{
		{Role: RoleView, TTL: time.Hour, IssuedBy: "admin"},
		{Role: RoleView, TTL: 24 * time.Hour, Name: "day", IssuedBy: "admin"},
		{Role: RoleView, Name: "forever", IssuedBy: "admin"},
	} {
		if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, req); err != nil {
			t.Fatal(err)
		}
	}
	revoked, err := RemoveExpired(ctx, testClient, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0].Role != RoleView || revoked[0].Expires == nil {
		t.Errorf("revoked %+v, want the expired view kubeconfig", revoked)
	}
	creds, err := List(ctx, testClient)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range creds {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "named-day,named-forever" {
		t.Errorf("credentials after the expiry pass: %v", names)
	}
}

func TestListDescribesCredentials(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleView, TTL: 3 * time.Hour, IssuedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	if err := RecordTalosconfig(ctx, testClient, RoleOSReader, notAfter, "bob"); err != nil {
		t.Fatal(err)
	}
	creds, err := List(ctx, testClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("%d credentials, want 2: %+v", len(creds), creds)
	}
	kube, talos := creds[0], creds[1]
	if kube.Kind != KindKubeconfig || kube.Role != RoleView || kube.IssuedBy != "alice" || kube.Expires == nil || kube.IssuedAt.IsZero() {
		t.Errorf("kubeconfig %+v", kube)
	}
	if talos.Kind != KindTalosconfig || talos.Role != RoleOSReader || talos.IssuedBy != "bob" || talos.Name != "" ||
		talos.Expires == nil || !talos.Expires.Equal(notAfter) {
		t.Errorf("talosconfig %+v", talos)
	}
}

func TestCorruptRecordsDoNotHoldUpKubeconfigs(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, KubeRequest{Role: RoleView, TTL: time.Hour, IssuedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: TalosconfigRecords}, Data: map[string]string{recordsKey: "not json"}}
	if err := testClient.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	creds, err := List(ctx, testClient)
	if !errors.Is(err, ErrCorruptRecords) || len(creds) != 1 || creds[0].Kind != KindKubeconfig {
		t.Errorf("List: %+v, %v; want the kubeconfig and ErrCorruptRecords", creds, err)
	}
	if _, err := RemoveExpired(ctx, testClient, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrCorruptRecords) {
		t.Errorf("RemoveExpired: %v, want ErrCorruptRecords", err)
	}
	list := &corev1.ServiceAccountList{}
	if err := testClient.List(ctx, list, client.InNamespace(Namespace), client.MatchingLabels{LabelKind: KindKubeconfig}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d expired ServiceAccounts kept", len(list.Items))
	}
}

// cancellingClient cancels the request when the ClusterRoleBinding is created, as a client that goes away would.
type cancellingClient struct {
	client.Client
	cancel context.CancelFunc
}

func (c cancellingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*rbacv1.ClusterRoleBinding); ok {
		c.cancel()
		return ctx.Err()
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestIssueCleansUpAfterTheRequestIsCancelled(t *testing.T) {
	resetAccess(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := cancellingClient{testClient, cancel}
	if _, _, err := IssueKubeconfig(ctx, c, testKube, testServer, testCA, KubeRequest{Role: RoleClusterAdmin, Name: "gone", IssuedBy: "admin"}); err == nil {
		t.Fatal("issued without a binding")
	}
	list := &corev1.ServiceAccountList{}
	if err := testClient.List(context.Background(), list, client.InNamespace(Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d ServiceAccounts left behind by a cancelled request", len(list.Items))
	}
}

// failingBindingDelete fails to delete the ClusterRoleBinding named name.
type failingBindingDelete struct {
	client.Client
	name string
}

func (c failingBindingDelete) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*rbacv1.ClusterRoleBinding); ok && obj.GetName() == c.name {
		return errors.New("etcd unavailable")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestRemoveExpiredGoesOnAfterAFailedRevoke(t *testing.T) {
	resetAccess(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		req := KubeRequest{Role: RoleView, TTL: time.Hour, Name: name, IssuedBy: "admin"}
		if _, _, err := IssueKubeconfig(ctx, testClient, testKube, testServer, testCA, req); err != nil {
			t.Fatal(err)
		}
	}
	c := failingBindingDelete{testClient, bindingName(namedPrefix + "a")}
	if _, err := RemoveExpired(ctx, c, time.Now().Add(2*time.Hour)); err == nil {
		t.Error("RemoveExpired hid the failed revoke")
	}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: namedPrefix + "b"}, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the second expired kubeconfig after a failed revoke of the first: %v", err)
	}
}
