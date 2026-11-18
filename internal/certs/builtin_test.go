package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var testClient client.Client

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		return 1
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "addons", "cert-manager.yaml")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		println("start envtest:", err.Error())
		return 1
	}
	defer func() { _ = env.Stop() }()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		println("scheme:", err.Error())
		return 1
	}
	if testClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		println("client:", err.Error())
		return 1
	}
	for _, ns := range []string{Namespace, ClusterResourceNamespace} {
		if err := testClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			println("namespace:", err.Error())
			return 1
		}
	}
	return m.Run()
}

func get(t *testing.T, kind, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk(kind))
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, u); err != nil {
		t.Fatalf("get %s %s/%s: %v", kind, namespace, name, err)
	}
	return u
}

func str(t *testing.T, u *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	v, _, err := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := v.(string)
	return s
}

func stringSlice(t *testing.T, u *unstructured.Unstructured, fields ...string) []string {
	t.Helper()
	v, _, err := unstructured.NestedStringSlice(u.Object, fields...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEnsureBuiltinCreatesChain(t *testing.T) {
	ctx := context.Background()
	hosts := []string{"appliance.example.com", "ilm.example.com"}
	ips := []netip.Addr{netip.MustParseAddr("192.0.2.10")}

	name, err := EnsureBuiltin(ctx, testClient, hosts, ips)
	if err != nil {
		t.Fatal(err)
	}
	if name != SecretName {
		t.Errorf("secret name %q, want %q", name, SecretName)
	}

	selfSigned := get(t, "ClusterIssuer", "", SelfSignedIssuerName)
	if _, ok, _ := unstructured.NestedMap(selfSigned.Object, "spec", "selfSigned"); !ok {
		t.Errorf("self-signed issuer spec %v", selfSigned.Object["spec"])
	}

	root := get(t, "Certificate", ClusterResourceNamespace, RootCAName)
	if ca, _, _ := unstructured.NestedBool(root.Object, "spec", "isCA"); !ca {
		t.Error("root certificate is not a CA")
	}
	for path, want := range map[string]string{
		"spec.secretName":                RootCAName,
		"spec.duration":                  "87600h",
		"spec.privateKey.algorithm":      "ECDSA",
		"spec.privateKey.rotationPolicy": "Never",
		"spec.issuerRef.name":            SelfSignedIssuerName,
		"spec.issuerRef.kind":            "ClusterIssuer",
	} {
		if got := str(t, root, strings.Split(path, ".")...); got != want {
			t.Errorf("root %s = %q, want %q", path, got, want)
		}
	}
	if size, _, _ := unstructured.NestedInt64(root.Object, "spec", "privateKey", "size"); size != 256 {
		t.Errorf("root key size %d, want 256", size)
	}

	ca := get(t, "ClusterIssuer", "", CAIssuerName)
	if got := str(t, ca, "spec", "ca", "secretName"); got != RootCAName {
		t.Errorf("CA issuer secret %q, want %q", got, RootCAName)
	}

	leaf := get(t, "Certificate", Namespace, CertificateName)
	for path, want := range map[string]string{
		"spec.secretName":           SecretName,
		"spec.duration":             "8760h",
		"spec.renewBefore":          "720h",
		"spec.privateKey.algorithm": "ECDSA",
		"spec.issuerRef.name":       CAIssuerName,
		"spec.issuerRef.kind":       "ClusterIssuer",
	} {
		if got := str(t, leaf, strings.Split(path, ".")...); got != want {
			t.Errorf("leaf %s = %q, want %q", path, got, want)
		}
	}
	if got := stringSlice(t, leaf, "spec", "dnsNames"); !reflect.DeepEqual(got, hosts) {
		t.Errorf("leaf dnsNames %v, want %v", got, hosts)
	}
	if got := stringSlice(t, leaf, "spec", "ipAddresses"); !reflect.DeepEqual(got, []string{"192.0.2.10"}) {
		t.Errorf("leaf ipAddresses %v", got)
	}
	for _, u := range []*unstructured.Unstructured{selfSigned, root, ca, leaf} {
		if !appliedBy(u, FieldManager) {
			t.Errorf("%s %s is not applied by %s: %+v", u.GetKind(), u.GetName(), FieldManager, u.GetManagedFields())
		}
	}

	// A second call changes nothing.
	if _, err := EnsureBuiltin(ctx, testClient, hosts, ips); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*unstructured.Unstructured{selfSigned, root, ca, leaf} {
		again := get(t, u.GetKind(), u.GetNamespace(), u.GetName())
		if again.GetResourceVersion() != u.GetResourceVersion() {
			t.Errorf("%s %s changed on the second call", u.GetKind(), u.GetName())
		}
	}

	// A new host name updates the leaf only.
	if _, err := EnsureBuiltin(ctx, testClient, hosts[:1], ips); err != nil {
		t.Fatal(err)
	}
	if got := stringSlice(t, get(t, "Certificate", Namespace, CertificateName), "spec", "dnsNames"); !reflect.DeepEqual(got, hosts[:1]) {
		t.Errorf("leaf dnsNames %v after the change, want %v", got, hosts[:1])
	}
	if again := get(t, "Certificate", ClusterResourceNamespace, RootCAName); again.GetResourceVersion() != root.GetResourceVersion() {
		t.Error("the root CA changed with the leaf's host names")
	}
}

func TestEnsureBuiltinNeedsAHost(t *testing.T) {
	if _, err := EnsureBuiltin(context.Background(), testClient, nil, nil); err == nil {
		t.Error("EnsureBuiltin without host names succeeded")
	}
}

func TestEnsureBuiltinReportsApplyErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EnsureBuiltin(ctx, testClient, []string{"a.example.com"}, nil); err == nil {
		t.Error("EnsureBuiltin with a cancelled context succeeded")
	}
}

func TestReady(t *testing.T) {
	ctx := context.Background()
	if _, err := EnsureBuiltin(ctx, testClient, []string{"ready.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if ready, err := Ready(ctx, testClient); err != nil || ready {
		t.Errorf("Ready = %v, %v before cert-manager issued, want false", ready, err)
	}

	setReady(t, "False", 0)
	if ready, _ := Ready(ctx, testClient); ready {
		t.Error("Ready with a False condition")
	}
	leaf := get(t, "Certificate", Namespace, CertificateName)
	setReady(t, "True", leaf.GetGeneration()-1)
	if ready, _ := Ready(ctx, testClient); ready {
		t.Error("Ready for an older generation")
	}
	setReady(t, "True", leaf.GetGeneration())
	if ready, err := Ready(ctx, testClient); err != nil || !ready {
		t.Errorf("Ready = %v, %v, want true", ready, err)
	}
}

func TestReadyReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Ready(ctx, testClient); err == nil {
		t.Error("Ready with a failing read succeeded")
	}
}

func setReady(t *testing.T, status string, generation int64) {
	t.Helper()
	leaf := get(t, "Certificate", Namespace, CertificateName)
	conditions := []any{map[string]any{
		"type": "Ready", "status": status, "reason": "Test", "message": "test",
		"observedGeneration": generation, "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}
	if err := unstructured.SetNestedSlice(leaf.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Status().Update(context.Background(), leaf); err != nil {
		t.Fatal(err)
	}
}

func TestRootCA(t *testing.T) {
	ctx := context.Background()
	if _, err := RootCA(ctx, testClient); !errors.Is(err, ErrNotIssued) {
		t.Fatalf("RootCA before issuance = %v, want ErrNotIssued", err)
	}

	caPEM := selfSignedPEM(t)
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not a real key")})
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: SecretName},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("leaf"),
			corev1.TLSPrivateKeyKey: key,
			caKey:                   append(append([]byte{}, caPEM...), key...),
		},
	}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, s) })

	got, err := RootCA(ctx, testClient)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(caPEM) {
		t.Errorf("RootCA = %q, want only the certificate %q", got, caPEM)
	}

	s.Data[caKey] = key
	if err := testClient.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := RootCA(ctx, testClient); !errors.Is(err, ErrNotIssued) {
		t.Errorf("RootCA without a certificate = %v, want ErrNotIssued", err)
	}
}

func TestRootCAReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RootCA(ctx, testClient); err == nil || errors.Is(err, ErrNotIssued) {
		t.Errorf("RootCA with a failing read = %v, want a read error", err)
	}
}

func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func appliedBy(u *unstructured.Unstructured, manager string) bool {
	for _, e := range u.GetManagedFields() {
		if e.Manager == manager && e.Operation == metav1.ManagedFieldsOperationApply {
			return true
		}
	}
	return false
}
