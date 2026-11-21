package access

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	talosx509 "github.com/siderolabs/crypto/x509"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIssueBreakGlass(t *testing.T) {
	resetAccess(t)
	fillTokens(t, "break-glass-token")
	ctx := context.Background()
	data, err := IssueBreakGlass(ctx, testClient, testKube, testServer, testCA, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, token := issued(t, data); token != "break-glass-token" {
		t.Errorf("token %q", token)
	}
	sa := account(t)
	if sa.Name != BreakGlass || sa.Annotations[AnnotationRole] != RoleClusterAdmin || sa.Annotations[AnnotationExpires] != "" {
		t.Errorf("ServiceAccount %s with annotations %v", sa.Name, sa.Annotations)
	}
	crb := &rbacv1.ClusterRoleBinding{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: Namespace + ":" + BreakGlass}, crb); err != nil {
		t.Fatal(err)
	}
	if crb.RoleRef.Name != RoleClusterAdmin {
		t.Errorf("bound to %s", crb.RoleRef.Name)
	}
	creds, err := List(ctx, testClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || !creds[0].BreakGlass || creds[0].Name != BreakGlass || creds[0].Expires != nil {
		t.Errorf("listed %+v", creds)
	}
}

func TestBreakGlassReissueReplacesTheAccount(t *testing.T) {
	resetAccess(t)
	fillTokens(t, "token")
	ctx := context.Background()
	if _, err := IssueBreakGlass(ctx, testClient, testKube, testServer, testCA, "admin"); err != nil {
		t.Fatal(err)
	}
	first := account(t)
	if _, err := IssueBreakGlass(ctx, testClient, testKube, testServer, testCA, "ops"); err != nil {
		t.Fatal(err)
	}
	second := account(t)
	if second.UID == first.UID || second.Annotations[AnnotationIssuedBy] != "ops" {
		t.Errorf("the ServiceAccount was not recreated: %s %v", second.UID, second.Annotations)
	}
	if owned := ownedSecrets(t, second); len(owned) != 1 || owned[0].Type != corev1.SecretTypeServiceAccountToken {
		t.Errorf("the new ServiceAccount owns the Secrets %v, want its token Secret", owned)
	}
}

// ownedSecrets returns the Secrets in Namespace that sa owns.
func ownedSecrets(t *testing.T, sa *corev1.ServiceAccount) []corev1.Secret {
	t.Helper()
	list := &corev1.SecretList{}
	if err := testClient.List(context.Background(), list, client.InNamespace(Namespace)); err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(list.Items, func(s corev1.Secret) bool {
		return !slices.ContainsFunc(s.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == sa.UID })
	})
}

func TestExpiryLeavesBreakGlass(t *testing.T) {
	resetAccess(t)
	fillTokens(t, "token")
	ctx := context.Background()
	if _, err := IssueBreakGlass(ctx, testClient, testKube, testServer, testCA, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveExpired(ctx, testClient, time.Now().Add(100*365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: BreakGlass}, &corev1.ServiceAccount{}); err != nil {
		t.Errorf("break-glass after the expiry sweep: %v", err)
	}
}

func TestBreakGlassFailureLeavesNoAccount(t *testing.T) {
	resetAccess(t)
	saved := tokenWait
	tokenWait = 50 * time.Millisecond
	t.Cleanup(func() { tokenWait = saved })
	if _, err := IssueBreakGlass(context.Background(), testClient, testKube, testServer, testCA, "admin"); err == nil {
		t.Fatal("issued without a token")
	}
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: Namespace, Name: BreakGlass}, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("ServiceAccount after a failed issue: %v", err)
	}
}

// certified returns a talosconfig whose client certificate expires at notAfter.
func certified(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{Organization: []string{RoleOSAdmin}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cfg := clientconfig.NewConfig("appliance", []string{"10.255.0.1"}, []byte("ca"), &talosx509.PEMEncodedCertificateAndKey{Crt: crt, Key: []byte("key")})
	data, err := cfg.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBreakGlassTalosconfig(t *testing.T) {
	notAfter := time.Now().Add(BreakGlassTalosconfigTTL).UTC().Truncate(time.Second)
	node := &fakeNode{data: certified(t, notAfter)}
	data, got, err := BreakGlassTalosconfig(context.Background(), node, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(node.roles, []string{RoleOSAdmin}) || node.ttl != 8760*time.Hour {
		t.Errorf("roles %v and TTL %v", node.roles, node.ttl)
	}
	if !got.Equal(notAfter) {
		t.Errorf("notAfter %v, want %v", got, notAfter)
	}
	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if endpoints := cfg.Contexts[cfg.Context].Endpoints; !slices.Equal(endpoints, []string{"192.0.2.10"}) {
		t.Errorf("endpoints %v", endpoints)
	}
	if _, _, err := BreakGlassTalosconfig(context.Background(), &fakeNode{data: generated(t)}, "192.0.2.10"); err == nil {
		t.Error("accepted a talosconfig without a certificate")
	}
}
