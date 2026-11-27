package certs

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const testDirectory = "https://acme.example.com/directory"

var acmeHosts = []string{"appliance.example.com", "ilm.example.com"}

// cleanACME removes the ACME objects and Secrets when the test ends; envtest runs no garbage collector.
func cleanACME(t *testing.T) {
	t.Cleanup(func() {
		ctx := context.Background()
		for _, kind := range []string{"Issuer", "Certificate"} {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk(kind))
			_ = testClient.DeleteAllOf(ctx, u, client.InNamespace(Namespace))
		}
		_ = testClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(Namespace))
	})
}

func ensureACME(t *testing.T, spec v1alpha1.ACMESpec, ips ...netip.Addr) (bool, string) {
	t.Helper()
	ready, problem, err := EnsureACME(context.Background(), testClient, spec, testDirectory, acmeHosts, ips)
	if err != nil {
		t.Fatal(err)
	}
	return ready, problem
}

func field(t *testing.T, u *unstructured.Unstructured, fields ...string) any {
	t.Helper()
	v, _, err := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func onlySolver(t *testing.T, issuer *unstructured.Unstructured) map[string]any {
	t.Helper()
	solvers, _, _ := unstructured.NestedSlice(issuer.Object, "spec", "acme", "solvers")
	if len(solvers) != 1 {
		t.Fatalf("solvers %v, want one", solvers)
	}
	return solvers[0].(map[string]any)
}

func TestEnsureACMEWithHTTP01(t *testing.T) {
	cleanACME(t)
	spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory, Email: "admin@example.com", Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01}}
	ready, problem := ensureACME(t, spec, netip.MustParseAddr("192.0.2.10"))
	if ready || problem != "" {
		t.Errorf("ready %v, problem %q; want an unissued certificate without a problem", ready, problem)
	}

	issuer := get(t, "Issuer", Namespace, ACMEIssuerName)
	for path, want := range map[string]string{
		"spec.acme.server":                   testDirectory,
		"spec.acme.email":                    "admin@example.com",
		"spec.acme.privateKeySecretRef.name": ACMEAccountSecretName,
	} {
		if got := str(t, issuer, strings.Split(path, ".")...); got != want {
			t.Errorf("issuer %s = %q, want %q", path, got, want)
		}
	}
	for _, absent := range [][]string{{"spec", "acme", "externalAccountBinding"}, {"spec", "acme", "caBundle"}} {
		if v := field(t, issuer, absent...); v != nil {
			t.Errorf("issuer %v = %v, want none", absent, v)
		}
	}
	want := map[string]any{"http01": map[string]any{"ingress": map[string]any{"ingressClassName": "traefik"}}}
	if got := onlySolver(t, issuer); !reflect.DeepEqual(got, want) {
		t.Errorf("solver %v, want %v", got, want)
	}

	cert := get(t, "Certificate", Namespace, ACMECertificateName)
	if got := str(t, cert, "spec", "secretName"); got != ACMESecretName {
		t.Errorf("secretName %q, want %q", got, ACMESecretName)
	}
	if got := stringSlice(t, cert, "spec", "dnsNames"); !reflect.DeepEqual(got, acmeHosts) {
		t.Errorf("dnsNames %v, want %v", got, acmeHosts)
	}
	wantRef := map[string]any{"name": ACMEIssuerName, "kind": "Issuer", "group": "cert-manager.io"}
	if got := field(t, cert, "spec", "issuerRef"); !reflect.DeepEqual(got, wantRef) {
		t.Errorf("issuerRef %v, want %v", got, wantRef)
	}
	for _, absent := range []string{"ipAddresses", "renewBefore"} {
		if v := field(t, cert, "spec", absent); v != nil {
			t.Errorf("certificate %s = %v, want none", absent, v)
		}
	}
}

func TestEnsureACMEWithBindingAndCABundle(t *testing.T) {
	cleanACME(t)
	bundle := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	spec := v1alpha1.ACMESpec{
		DirectoryURL: testDirectory, EABKeyID: "kid-1",
		EABSecretRef: &corev1.LocalObjectReference{Name: ACMEEABSecretName}, CABundle: bundle,
	}
	ensureACME(t, spec)

	issuer := get(t, "Issuer", Namespace, ACMEIssuerName)
	wantEAB := map[string]any{"keyID": "kid-1", "keySecretRef": map[string]any{"name": ACMEEABSecretName, "key": ACMEEABHMACKey}}
	if got := field(t, issuer, "spec", "acme", "externalAccountBinding"); !reflect.DeepEqual(got, wantEAB) {
		t.Errorf("externalAccountBinding %v, want %v", got, wantEAB)
	}
	if got := str(t, issuer, "spec", "acme", "caBundle"); got != base64.StdEncoding.EncodeToString(bundle) {
		t.Errorf("caBundle %q, want the bundle", got)
	}
	if v := field(t, issuer, "spec", "acme", "email"); v != nil {
		t.Errorf("email %v, want none without one", v)
	}

	// Applying the settings without the binding removes it.
	ensureACME(t, v1alpha1.ACMESpec{DirectoryURL: testDirectory})
	if v := field(t, get(t, "Issuer", Namespace, ACMEIssuerName), "spec", "acme", "externalAccountBinding"); v != nil {
		t.Errorf("externalAccountBinding %v, want none once removed", v)
	}
}

func TestEnsureACMEWithEveryDNS01Provider(t *testing.T) {
	providers := slices.Sorted(func(yield func(string) bool) {
		for name := range DNS01Providers {
			if !yield(name) {
				return
			}
		}
	})
	solverField := map[string]string{"azuredns": "azureDNS", "clouddns": "cloudDNS", "acmedns": "acmeDNS"}
	secretField := map[string]string{"serviceAccountKey": "serviceAccountSecretRef", "accountJSON": "accountSecretRef"}
	for _, name := range providers {
		t.Run(name, func(t *testing.T) {
			cleanACME(t)
			p := DNS01Providers[name]
			config, credentials := map[string]string{}, map[string][]byte{}
			want := map[string]any{}
			for _, k := range p.Config {
				config[k.Name] = "value-" + k.Name
				want[k.Name] = "value-" + k.Name
			}
			for _, k := range p.Secrets {
				credentials[k.Name] = []byte("secret")
				f := secretField[k.Name]
				if f == "" {
					f = k.Name + "SecretRef"
				}
				want[f] = map[string]any{"name": ACMEDNS01SecretName, "key": k.Name}
			}
			writeSecret(t, ACMEDNS01SecretName, credentials)
			spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory, Solver: v1alpha1.ACMESolver{
				Type: v1alpha1.ACMESolverDNS01,
				DNS01: &v1alpha1.DNS01Solver{
					Provider: name, Config: config, CredentialsSecretRef: &corev1.LocalObjectReference{Name: ACMEDNS01SecretName},
				},
			}}
			ensureACME(t, spec)

			f := solverField[name]
			if f == "" {
				f = name
			}
			wantSolver := map[string]any{"dns01": map[string]any{f: want}}
			if got := onlySolver(t, get(t, "Issuer", Namespace, ACMEIssuerName)); !reflect.DeepEqual(got, wantSolver) {
				t.Errorf("solver %v, want %v", got, wantSolver)
			}
		})
	}
}

func TestEnsureACMEReferencesOnlyStoredCredentials(t *testing.T) {
	cleanACME(t)
	// azuredns with a managed identity has no client secret.
	spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory, Solver: v1alpha1.ACMESolver{
		Type: v1alpha1.ACMESolverDNS01,
		DNS01: &v1alpha1.DNS01Solver{Provider: "azuredns", Config: map[string]string{
			"subscriptionID": "sub", "resourceGroupName": "rg",
		}},
	}}
	ensureACME(t, spec)
	want := map[string]any{"dns01": map[string]any{"azureDNS": map[string]any{"subscriptionID": "sub", "resourceGroupName": "rg"}}}
	if got := onlySolver(t, get(t, "Issuer", Namespace, ACMEIssuerName)); !reflect.DeepEqual(got, want) {
		t.Errorf("solver %v, want %v", got, want)
	}
}

func TestEnsureACMERefusesAnUnknownDNSProvider(t *testing.T) {
	spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory, Solver: v1alpha1.ACMESolver{
		Type: v1alpha1.ACMESolverDNS01, DNS01: &v1alpha1.DNS01Solver{Provider: "unknown"},
	}}
	if _, _, err := EnsureACME(context.Background(), testClient, spec, testDirectory, acmeHosts, nil); err == nil {
		t.Error("an unknown DNS provider was rendered")
	}
}

func TestEnsureACMEForTheILMPreset(t *testing.T) {
	cleanACME(t)
	spec := v1alpha1.ACMESpec{
		ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: "ra", VaultUUID: "v", VaultProfileUUID: "vp", ProfileName: "appliance"},
		EABKeyID:   "kid-ilm",
		// The preset ignores the solver.
		Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01},
	}
	ensureACME(t, spec, netip.MustParseAddr("192.0.2.10"))

	issuer := get(t, "Issuer", Namespace, ACMEIssuerName)
	if got := str(t, issuer, "spec", "acme", "server"); got != testDirectory {
		t.Errorf("server %q, want the directory passed in", got)
	}
	if v := field(t, issuer, "spec", "acme", "solvers"); v != nil {
		t.Errorf("solvers %v, want none for the ILM preset", v)
	}
	if v := field(t, issuer, "spec", "acme", "caBundle"); v != nil {
		t.Errorf("caBundle %v, want none without spec.caBundle", v)
	}
	wantEAB := map[string]any{"keyID": "kid-ilm", "keySecretRef": map[string]any{"name": ACMEEABSecretName, "key": ACMEEABHMACKey}}
	if got := field(t, issuer, "spec", "acme", "externalAccountBinding"); !reflect.DeepEqual(got, wantEAB) {
		t.Errorf("externalAccountBinding %v, want %v", got, wantEAB)
	}
	cert := get(t, "Certificate", Namespace, ACMECertificateName)
	if got := stringSlice(t, cert, "spec", "ipAddresses"); !reflect.DeepEqual(got, []string{"192.0.2.10"}) {
		t.Errorf("ipAddresses %v, want the node address", got)
	}
}

// setStatus writes conditions, and lastFailureTime when failed, into the status of the ACME object kind.
func setStatus(t *testing.T, kind, name, status string, failed bool) {
	t.Helper()
	u := get(t, kind, Namespace, name)
	conditions := []any{map[string]any{
		"type": "Ready", "status": status, "reason": "Test", "message": "server answer marker",
		"observedGeneration": u.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}
	if err := unstructured.SetNestedSlice(u.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if failed {
		if err := unstructured.SetNestedField(u.Object, time.Now().UTC().Format(time.RFC3339), "status", "lastFailureTime"); err != nil {
			t.Fatal(err)
		}
	}
	if err := testClient.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureACMEReportsTheCertificate(t *testing.T) {
	http01 := v1alpha1.ACMESpec{DirectoryURL: testDirectory}
	dns01 := v1alpha1.ACMESpec{DirectoryURL: testDirectory, Solver: v1alpha1.ACMESolver{
		Type: v1alpha1.ACMESolverDNS01, DNS01: &v1alpha1.DNS01Solver{Provider: "rfc2136", Config: map[string]string{"nameserver": "192.0.2.53"}},
	}}
	preset := v1alpha1.ACMESpec{ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: "ra", VaultUUID: "v", VaultProfileUUID: "vp"}}
	for _, tc := range []struct {
		name        string
		spec        v1alpha1.ACMESpec
		issuer      string
		certificate string
		failed      bool
		ready       bool
		problem     string
	}{
		{name: "issuing", spec: http01, issuer: "True", certificate: "False"},
		{name: "ready", spec: http01, issuer: "True", certificate: "True", ready: true},
		{name: "renewal failed", spec: http01, issuer: "True", certificate: "True", failed: true, problem: ProblemHTTP01},
		{name: "http01 failed", spec: http01, issuer: "True", certificate: "False", failed: true, problem: ProblemHTTP01},
		{name: "dns01 failed", spec: dns01, issuer: "True", certificate: "False", failed: true, problem: ProblemDNS01},
		{name: "preset failed", spec: preset, issuer: "True", certificate: "False", failed: true, problem: ProblemRefused},
		{name: "account refused", spec: http01, issuer: "False", certificate: "False", problem: ProblemRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanACME(t)
			ensureACME(t, tc.spec)
			setStatus(t, "Issuer", ACMEIssuerName, tc.issuer, false)
			setStatus(t, "Certificate", ACMECertificateName, tc.certificate, tc.failed)
			ready, problem := ensureACME(t, tc.spec)
			if ready != tc.ready || problem != tc.problem {
				t.Errorf("ready %v, problem %q; want %v, %q", ready, problem, tc.ready, tc.problem)
			}
		})
	}
}

// pemPair returns a fresh certificate that expires at notAfter and its key in PEM.
func pemPair(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	pair, err := SelfSigned("acme", acmeHosts, notAfter.Add(-2*time.Hour), notAfter)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeSecret(t *testing.T, name string, data map[string][]byte) {
	t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: name}, Data: data}
	if err := testClient.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func TestServedSecretName(t *testing.T) {
	certPEM, keyPEM := pemPair(t, time.Now().Add(time.Hour))
	valid := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: ACMESecretName},
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
	invalid := valid.DeepCopy()
	invalid.Data[corev1.TLSPrivateKeyKey] = []byte("no key")
	expired := valid.DeepCopy()
	expired.Data[corev1.TLSCertKey], expired.Data[corev1.TLSPrivateKeyKey] = pemPair(t, time.Now().Add(-time.Minute))
	acme := v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: &v1alpha1.ACMESpec{DirectoryURL: testDirectory}}
	for _, tc := range []struct {
		name   string
		tls    v1alpha1.TLSSpec
		secret *corev1.Secret
		// host is the ILM host name, by default one the certificate covers.
		host string
		want string
	}{
		{name: "builtin", tls: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin}, secret: valid, want: SecretName},
		{name: "secret", tls: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "ilm-tls"}, secret: valid, want: "ilm-tls"},
		{name: "acme not issued", tls: acme, want: SecretName},
		{name: "acme invalid", tls: acme, secret: invalid, want: SecretName},
		{name: "acme expired", tls: acme, secret: expired, want: SecretName},
		{name: "acme issued", tls: acme, secret: valid, want: ACMESecretName},
		{name: "acme for other host names", tls: acme, secret: valid, host: "pki.example.com", want: SecretName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder()
			if tc.secret != nil {
				b = b.WithObjects(tc.secret)
			}
			a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{TLS: tc.tls, Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{{HostName: acmeHosts[0]}}},
				Platform: &v1alpha1.PlatformSpec{HostName: cmp.Or(tc.host, acmeHosts[1])}}}
			got, err := ServedSecretName(context.Background(), b.Build(), a)
			if err != nil || got != tc.want {
				t.Errorf("ServedSecretName = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func issuing(t *testing.T) string {
	t.Helper()
	conditions, _, _ := unstructured.NestedSlice(get(t, "Certificate", Namespace, ACMECertificateName).Object, "status", "conditions")
	for _, item := range conditions {
		if cond := item.(map[string]any); cond["type"] == "Issuing" {
			return cond["status"].(string)
		}
	}
	return ""
}

func TestEnsureACMERetriesAFailedIssuanceAfterASettingsChange(t *testing.T) {
	cleanACME(t)
	spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory, EABKeyID: "kid", EABSecretRef: &corev1.LocalObjectReference{Name: ACMEEABSecretName}}
	writeSecret(t, ACMEEABSecretName, map[string][]byte{ACMEEABHMACKey: []byte("first")})
	ensureACME(t, spec)
	setStatus(t, "Certificate", ACMECertificateName, "False", true)

	// Unchanged settings leave cert-manager's backoff alone.
	if _, problem := ensureACME(t, spec); problem != ProblemHTTP01 || issuing(t) != "" {
		t.Errorf("problem %q, Issuing %q; want the problem and no retry for unchanged settings", problem, issuing(t))
	}

	// A changed Secret the Issuer references triggers one retry.
	s := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: Namespace, Name: ACMEEABSecretName}, s); err != nil {
		t.Fatal(err)
	}
	s.Data[ACMEEABHMACKey] = []byte("corrected")
	if err := testClient.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if ready, problem := ensureACME(t, spec); ready || problem != "" || issuing(t) != "True" {
		t.Errorf("ready %v, problem %q, Issuing %q; want a retry reported as issuing", ready, problem, issuing(t))
	}
	if got := get(t, "Certificate", Namespace, ACMECertificateName).GetAnnotations()[IssuerFingerprintAnnotation]; got == "" {
		t.Error("the fingerprint is not recorded")
	}

	// Once recorded, the same settings do not retry again.
	setStatus(t, "Certificate", ACMECertificateName, "False", true)
	if _, problem := ensureACME(t, spec); problem != ProblemHTTP01 || issuing(t) != "" {
		t.Errorf("problem %q, Issuing %q; want no second retry", problem, issuing(t))
	}

	// So does a changed Issuer setting.
	spec.Email = "admin@example.com"
	if _, problem := ensureACME(t, spec); problem != "" || issuing(t) != "True" {
		t.Errorf("problem %q, Issuing %q; want a retry after the email changed", problem, issuing(t))
	}
}

func TestEnsureACMEReissuesAnIssuedCertificateAfterASettingsChange(t *testing.T) {
	cleanACME(t)
	spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory}
	ensureACME(t, spec)
	setStatus(t, "Certificate", ACMECertificateName, "True", false)

	if ready, _ := ensureACME(t, spec); !ready || issuing(t) != "" {
		t.Errorf("ready %v, Issuing %q; want the issued certificate left alone", ready, issuing(t))
	}

	// Changed Issuer settings, as another ACME server would be, issue the certificate again now.
	spec.Email = "admin@example.com"
	if ready, problem := ensureACME(t, spec); ready || problem != "" || issuing(t) != "True" {
		t.Errorf("ready %v, problem %q, Issuing %q; want a new issuance", ready, problem, issuing(t))
	}
}

func TestEnsureACMERetriesAfterTheILMProfileChanged(t *testing.T) {
	cleanACME(t)
	spec := v1alpha1.ACMESpec{ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: "ra1", VaultUUID: "v1", VaultProfileUUID: "vp1"}}
	ensureACME(t, spec)
	setStatus(t, "Certificate", ACMECertificateName, "False", true)

	// The Issuer is the same, but ILM's ACME profile now issues through another RA profile.
	spec.ILMProfile.RAProfileUUID = "ra2"
	if _, problem := ensureACME(t, spec); problem != "" || issuing(t) != "True" {
		t.Errorf("problem %q, Issuing %q; want a retry after the RA profile changed", problem, issuing(t))
	}
}

func TestRemoveACME(t *testing.T) {
	cleanACME(t)
	ctx := context.Background()
	ensureACME(t, v1alpha1.ACMESpec{DirectoryURL: testDirectory})
	names := []string{ACMESecretName, ACMEAccountSecretName, ACMEILMEABSecretName}
	kept := []string{ACMEDNS01SecretName, ACMEEABSecretName, SecretName}
	for _, name := range append(slices.Clone(names), kept...) {
		writeSecret(t, name, map[string][]byte{"k": []byte("v")})
	}

	for range 2 { // what is gone already is skipped
		if err := RemoveACME(ctx, testClient); err != nil {
			t.Fatal(err)
		}
	}
	for kind, name := range map[string]string{"Issuer": ACMEIssuerName, "Certificate": ACMECertificateName} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk(kind))
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, u); !apierrors.IsNotFound(err) {
			t.Errorf("%s after RemoveACME: %v", kind, err)
		}
	}
	for _, name := range names {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Errorf("Secret %s after RemoveACME: %v", name, err)
		}
	}
	// The console writes these before it selects source acme, so they outlive a pass that still sees another source.
	for _, name := range kept {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, &corev1.Secret{}); err != nil {
			t.Errorf("Secret %s after RemoveACME: %v", name, err)
		}
	}
}

// failingStatus is a client whose status updates fail, as on a conflict with cert-manager's own status write.
type failingStatus struct{ client.Client }

func (f failingStatus) Status() client.SubResourceWriter { return failingWriter{f.Client.Status()} }

type failingWriter struct{ client.SubResourceWriter }

func (failingWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("conflict")
}

func TestEnsureACMERetriesAgainAfterAFailedRetry(t *testing.T) {
	cleanACME(t)
	spec := v1alpha1.ACMESpec{DirectoryURL: testDirectory}
	ensureACME(t, spec)
	setStatus(t, "Certificate", ACMECertificateName, "False", true)

	spec.Email = "admin@example.com"
	conflicting := failingStatus{testClient}
	if _, _, err := EnsureACME(context.Background(), conflicting, spec, testDirectory, acmeHosts, nil); err == nil {
		t.Fatal("the failed retry was not reported")
	}
	if issuing(t) != "" {
		t.Fatal("Issuing set although the status update failed")
	}

	if _, problem := ensureACME(t, spec); problem != "" || issuing(t) != "True" {
		t.Errorf("problem %q, Issuing %q; want the next pass to retry", problem, issuing(t))
	}
	// The recorded fingerprint now stops further retries.
	setStatus(t, "Certificate", ACMECertificateName, "False", true)
	if _, problem := ensureACME(t, spec); problem != ProblemHTTP01 || issuing(t) != "" {
		t.Errorf("problem %q, Issuing %q; want no further retry", problem, issuing(t))
	}
}
