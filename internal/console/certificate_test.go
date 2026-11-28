package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

func fakeClient(objs ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func appliance(tlsSpec v1alpha1.TLSSpec) *v1alpha1.Appliance {
	return &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec:       v1alpha1.ApplianceSpec{Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{{HostName: "appliance.example.com"}}}, TLS: tlsSpec},
	}
}

// tlsSecret returns a TLS Secret in the system namespace with a fresh certificate for commonName.
func tlsSecret(t *testing.T, name, commonName string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: commonName},
		DNSNames: []string{"appliance.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		},
	}
}

func servedName(t *testing.T, c *Certificate) string {
	t.Helper()
	cert, err := c.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestCertificateIsSelfSignedFirst(t *testing.T) {
	c, err := NewCertificate(discard, fakeClient(appliance(v1alpha1.TLSSpec{})))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "ILM Appliance" {
		t.Errorf("served %q, want the self-signed certificate", got)
	}
}

func TestCertificateFollowsTheBuiltinSecret(t *testing.T) {
	ctx := context.Background()
	cl := fakeClient(appliance(v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin}), tlsSecret(t, certs.SecretName, "first"))
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "first" {
		t.Errorf("served %q, want first", got)
	}

	renewed := tlsSecret(t, certs.SecretName, "renewed")
	if err := cl.Update(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "renewed" {
		t.Errorf("served %q after renewal, want renewed", got)
	}
}

func TestCertificateFollowsTheSecretSource(t *testing.T) {
	cl := fakeClient(
		appliance(v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "uploaded"}),
		tlsSecret(t, certs.SecretName, "builtin"),
		tlsSecret(t, "uploaded", "uploaded"),
	)
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "uploaded" {
		t.Errorf("served %q, want uploaded", got)
	}
}

func TestCertificateFollowsTheACMESecretOnceValid(t *testing.T) {
	ctx := context.Background()
	acme := v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: &v1alpha1.ACMESpec{DirectoryURL: "https://acme.example.com/directory"}}
	cl := fakeClient(appliance(acme), tlsSecret(t, certs.SecretName, "builtin"), brokenSecret(t, certs.ACMESecretName))
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("an invalid ACME Secret must fall back to the built-in certificate: %v", err)
	}
	if got := servedName(t, c); got != "builtin" {
		t.Errorf("served %q, want builtin until the ACME certificate is issued", got)
	}

	if err := cl.Update(ctx, tlsSecret(t, certs.ACMESecretName, "acme")); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "acme" {
		t.Errorf("served %q, want the issued ACME certificate", got)
	}
}

func completeAppliance(status string) *v1alpha1.Appliance {
	a := appliance(v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin})
	a.Status.Setup = status
	return a
}

func TestCertificateServesTheSetupSecretUntilSetupIsComplete(t *testing.T) {
	ctx := context.Background()
	for name, objs := range map[string][]client.Object{
		"no Appliance":  {tlsSecret(t, certs.SetupSecretName, "setup")},
		"part 2":        {completeAppliance(v1alpha1.SetupPart2), tlsSecret(t, certs.SetupSecretName, "setup"), tlsSecret(t, certs.SecretName, "builtin")},
		"fresh status":  {completeAppliance(""), tlsSecret(t, certs.SetupSecretName, "setup"), tlsSecret(t, certs.SecretName, "builtin")},
		"broken secret": {completeAppliance(v1alpha1.SetupPart2), brokenSecret(t, certs.SetupSecretName), tlsSecret(t, certs.SecretName, "builtin")},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := NewCertificate(discard, fakeClient(objs...))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			want := "setup"
			if name == "broken secret" {
				want = "builtin"
			}
			if got := servedName(t, c); got != want {
				t.Errorf("served %q, want %q", got, want)
			}
		})
	}
}

func brokenSecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	s := tlsSecret(t, name, "broken")
	s.Data[corev1.TLSPrivateKeyKey] = []byte("not a key")
	return s
}

func TestCertificateLeavesTheSetupSecretOnceSetupIsComplete(t *testing.T) {
	cl := fakeClient(completeAppliance(v1alpha1.SetupComplete), tlsSecret(t, certs.SetupSecretName, "setup"), tlsSecret(t, certs.SecretName, "builtin"))
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "builtin" {
		t.Errorf("served %q, want the selected Secret", got)
	}
}

func TestCertificateKeepsTheLastGoodOne(t *testing.T) {
	ctx := context.Background()
	cl := fakeClient(appliance(v1alpha1.TLSSpec{}), tlsSecret(t, certs.SecretName, "good"))
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	broken := tlsSecret(t, certs.SecretName, "broken")
	broken.Data[corev1.TLSPrivateKeyKey] = []byte("not a key")
	if err := cl.Update(ctx, broken); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err == nil {
		t.Error("a broken Secret must be reported")
	}
	if err := cl.Delete(ctx, broken); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "good" {
		t.Errorf("served %q, want the last good certificate", got)
	}
}

func TestCertificateWithoutTheAppliance(t *testing.T) {
	c, err := NewCertificate(discard, fakeClient())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := servedName(t, c); got != "ILM Appliance" {
		t.Errorf("served %q, want the self-signed certificate", got)
	}
}

func TestCertificateReportsReadErrors(t *testing.T) {
	cl := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("API server down")
		},
	}).Build()
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err == nil {
		t.Error("a read error must be reported")
	}
}

func TestCertificateRunRefreshesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cl := fakeClient(appliance(v1alpha1.TLSSpec{}))
	c, err := NewCertificate(discard, cl)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, 10*time.Millisecond) }()
	if err := cl.Create(ctx, tlsSecret(t, certs.SecretName, "issued")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for servedName(t, c) != "issued" {
		if time.Now().After(deadline) {
			t.Fatal("Run did not pick up the issued certificate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
