package manager

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

func certManagerObject(kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: kind})
	return u
}

// cleanCertificates removes the built-in chain when the test ends; envtest runs no garbage collector.
func cleanCertificates(t *testing.T) {
	t.Cleanup(func() {
		ctx := context.Background()
		for _, ns := range []string{certs.Namespace, certs.ClusterResourceNamespace} {
			_ = testClient.DeleteAllOf(ctx, certManagerObject("Certificate"), client.InNamespace(ns))
		}
		_ = testClient.DeleteAllOf(ctx, certManagerObject("ClusterIssuer"))
		_ = testClient.DeleteAllOf(ctx, certManagerObject("Issuer"), client.InNamespace(certs.Namespace))
	})
}

func leafCertificate(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u := certManagerObject("Certificate")
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.CertificateName}, u); err != nil {
		t.Fatalf("leaf certificate: %v", err)
	}
	return u
}

// issue does what cert-manager does once it issued the leaf: it writes the Secret and reports the Certificate Ready.
func issue(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.SecretName},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: []byte("leaf"), corev1.TLSPrivateKeyKey: []byte("key"), "ca.crt": []byte("root"),
		},
	}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	leaf := leafCertificate(t)
	conditions := []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Ready", "message": "issued",
		"observedGeneration": leaf.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}
	if err := unstructured.SetNestedSlice(leaf.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Status().Update(ctx, leaf); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileIssuesTheBuiltinCertificate(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		Platform: &v1alpha1.PlatformSpec{HostName: "pki.example.com"},
	})
	r, _ := newReconciler(testClient, &fakeCluster{})

	if res := runReconcile(t, r); res.RequeueAfter != addonsRequeue {
		t.Errorf("requeue after %v, want %v while the certificate is issued", res.RequeueAfter, addonsRequeue)
	}
	leaf := leafCertificate(t)
	if dns, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "dnsNames"); !reflect.DeepEqual(dns, []string{"appliance.example.com", "pki.example.com"}) {
		t.Errorf("dnsNames %v, want the node and the ILM host name", dns)
	}
	if ips, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"192.0.2.10"}) {
		t.Errorf("ipAddresses %v, want the node address", ips)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonIssuing {
		t.Errorf("CertificateReady = %+v, want False while issuing", c)
	}
	if tls := getPlatform(t).Spec.Edge.TLS; tls.Source != "internal" {
		t.Errorf("edge TLS = %+v, want internal until the certificate is issued", tls)
	}

	issue(t)
	runReconcile(t, r)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionTrue || c.Reason != reasonReady {
		t.Errorf("CertificateReady = %+v, want True", c)
	}
	copied := secret(t, TLSSecretName)
	if copied.Type != corev1.SecretTypeTLS || len(copied.Data) != 2 || string(copied.Data[corev1.TLSCertKey]) != "leaf" {
		t.Errorf("TLS copy = %s with keys %v, want the issued pair only", copied.Type, slices.Collect(maps.Keys(copied.Data)))
	}
	if tls := getPlatform(t).Spec.Edge.TLS; tls.Source != "secret" || tls.SecretRef == nil || *tls.SecretRef != TLSSecretName {
		t.Errorf("edge TLS = %+v, want the copied built-in Secret", tls)
	}
}

func TestReconcileCertificateWaitsForCertManager(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"}})
	r, _ := newReconciler(testClient, &fakeCluster{unready: "cert-manager"})
	runReconcile(t, r)

	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForCertManager {
		t.Errorf("CertificateReady = %+v, want False waiting for cert-manager", c)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.CertificateName}, certManagerObject("Certificate")); err == nil {
		t.Error("the certificate was requested before cert-manager is ready")
	}
}

func TestReconcileCertificateWaitsForTheNodeAddress(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "appliance.example.com"}})
	r, _ := newReconciler(testClient, &fakeCluster{})
	if res := runReconcile(t, r); res.RequeueAfter != addonsRequeue {
		t.Errorf("requeue after %v, want %v: Nodes are not watched, so the address is looked for again", res.RequeueAfter, addonsRequeue)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForNodeAddress {
		t.Errorf("CertificateReady = %+v, want False waiting for the node address", c)
	}

	// With DHCP the address comes from the node.
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "talos-node"}}
	if err := testClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, node) })
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: "talos-node"},
		{Type: corev1.NodeInternalIP, Address: "169.254.1.1"},
		{Type: corev1.NodeInternalIP, Address: "198.51.100.7"},
	}
	if err := testClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if ips, _, _ := unstructured.NestedStringSlice(leafCertificate(t).Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"198.51.100.7"}) {
		t.Errorf("ipAddresses %v, want the node's usable internal address", ips)
	}

	// The status address, once reported, wins.
	a := getAppliance(t)
	a.Status.NodeAddress = "203.0.113.5"
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if ips, _, _ := unstructured.NestedStringSlice(leafCertificate(t).Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"203.0.113.5"}) {
		t.Errorf("ipAddresses %v, want the status address", ips)
	}
}

func TestReconcileSecretSourceHasNoBuiltinCertificate(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node: v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		TLS:  v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "ilm-tls"},
	})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	if c := meta.FindStatusCondition(getAppliance(t).Status.Conditions, v1alpha1.ConditionCertificateReady); c != nil {
		t.Errorf("CertificateReady = %+v, want none for an uploaded certificate", c)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.CertificateName}, certManagerObject("Certificate")); err == nil {
		t.Error("the built-in certificate was requested for an uploaded one")
	}
}

const marker = "marker-3e7c1"

func TestConditionsCarryNoErrorText(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: marker},
		Platform: platformSpec(),
	})
	cluster := &fakeCluster{applyErr: errors.New("apply failed: " + marker)}
	r, _ := newReconciler(testClient, cluster)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}}

	if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("err = %v, want the add-on error for the log", err)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Reason != reasonFailed || strings.Contains(c.Message, marker) {
		t.Errorf("AddonsReady = %+v, want a fixed reason and message", c)
	}

	cluster.applyErr = nil
	if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("err = %v, want the platform error for the log", err)
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady)
	if c.Reason != reasonFailed || strings.Contains(c.Message, marker) {
		t.Errorf("PlatformReady = %+v, want a fixed reason and message", c)
	}
	for _, c := range getAppliance(t).Status.Conditions {
		if strings.Contains(c.Message, marker) {
			t.Errorf("condition %s carries the error text: %q", c.Type, c.Message)
		}
	}
}

func TestCertificateRunsWithTheManagerRBAC(t *testing.T) {
	cleanCertificates(t)
	ctx := context.Background()
	c := managerClient(t)
	if _, err := certs.EnsureBuiltin(ctx, c, []string{"appliance.example.com"}, nil); err != nil {
		t.Fatalf("request the certificate as the manager: %v", err)
	}
	if _, err := certs.Ready(ctx, c); err != nil {
		t.Errorf("read the certificate as the manager: %v", err)
	}
	if _, err := certs.RootCA(ctx, c); !errors.Is(err, certs.ErrNotIssued) {
		t.Errorf("read the root CA as the manager: %v", err)
	}
	dns01 := v1alpha1.ACMESpec{DirectoryURL: "https://acme.example.com/directory", Solver: v1alpha1.ACMESolver{
		Type: v1alpha1.ACMESolverDNS01, DNS01: &v1alpha1.DNS01Solver{Provider: "cloudflare",
			CredentialsSecretRef: &corev1.LocalObjectReference{Name: certs.ACMEDNS01SecretName}},
	}}
	if _, _, err := certs.EnsureACME(ctx, c, dns01, dns01.DirectoryURL, []string{"appliance.example.com"}, nil); err != nil {
		t.Errorf("request the ACME certificate as the manager: %v", err)
	}
	// A failed issuance is retried after a settings change.
	setACMEStatus(t, false, true)
	dns01.Email = "admin@example.com"
	if _, _, err := certs.EnsureACME(ctx, c, dns01, dns01.DirectoryURL, []string{"appliance.example.com"}, nil); err != nil {
		t.Errorf("retry the ACME certificate as the manager: %v", err)
	}
	if err := certs.RemoveACME(ctx, c); err != nil {
		t.Errorf("remove the ACME certificate as the manager: %v", err)
	}
}

const acmeDirectory = "https://acme.example.com/directory"

func acmeCertificate(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u := certManagerObject("Certificate")
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMECertificateName}, u); err != nil {
		t.Fatalf("ACME certificate: %v", err)
	}
	return u
}

// setACMEStatus does what cert-manager does after an attempt to issue the ACME certificate: it reports it Ready or
// not, with lastFailureTime once an attempt failed.
func setACMEStatus(t *testing.T, ready, failed bool) {
	t.Helper()
	u := acmeCertificate(t)
	status := "False"
	if ready {
		status = "True"
	}
	conditions := []any{map[string]any{
		"type": "Ready", "status": status, "reason": "Test", "message": "server answer " + marker,
		"observedGeneration": u.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}
	if err := unstructured.SetNestedSlice(u.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if failed {
		if err := unstructured.SetNestedField(u.Object, time.Now().UTC().Format(time.RFC3339), "status", "lastFailureTime"); err != nil {
			t.Fatal(err)
		}
	} else {
		unstructured.RemoveNestedField(u.Object, "status", "lastFailureTime")
	}
	if err := testClient.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

// writeACMESecret writes the ACME certificate's Secret as cert-manager does once it issued it.
func writeACMESecret(t *testing.T) []byte {
	t.Helper()
	pair, err := certs.SelfSigned("acme", []string{"appliance.example.com", "pki.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]})
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.ACMESecretName},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		},
	}
	if err := testClient.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return certPEM
}

func TestReconcileACMEServesTheBuiltinCertificateUntilIssued(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		Platform: &v1alpha1.PlatformSpec{HostName: "pki.example.com"},
		TLS: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: &v1alpha1.ACMESpec{
			DirectoryURL: acmeDirectory, Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01},
		}},
	})
	r, _ := newReconciler(testClient, &fakeCluster{})

	if res := runReconcile(t, r); res.RequeueAfter != addonsRequeue {
		t.Errorf("requeue after %v, want %v while the certificate is issued", res.RequeueAfter, addonsRequeue)
	}
	if dns, _, _ := unstructured.NestedStringSlice(acmeCertificate(t).Object, "spec", "dnsNames"); !reflect.DeepEqual(dns, []string{"appliance.example.com", "pki.example.com"}) {
		t.Errorf("ACME dnsNames %v, want the node and the ILM host name", dns)
	}
	leafCertificate(t) // the built-in certificate stays the fallback
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonIssuing {
		t.Errorf("CertificateReady = %+v, want False while issuing", c)
	}

	// The built-in certificate is served while the ACME one fails.
	issue(t)
	setACMEStatus(t, false, true)
	runReconcile(t, r)
	c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady)
	if c.Status != metav1.ConditionFalse || c.Reason != reasonFailed || c.Message != certs.ProblemHTTP01 {
		t.Errorf("CertificateReady = %+v, want False with the HTTP-01 problem", c)
	}
	if got := string(secret(t, TLSSecretName).Data[corev1.TLSCertKey]); got != "leaf" {
		t.Errorf("served %q, want the built-in certificate", got)
	}
	if p := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady); p.Reason == reasonFailed {
		t.Errorf("PlatformReady = %+v, want the ACME failure not to fail the platform", p)
	}

	issued := writeACMESecret(t)
	setACMEStatus(t, true, false)
	runReconcile(t, r)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionTrue || c.Reason != reasonReady {
		t.Errorf("CertificateReady = %+v, want True", c)
	}
	if got := secret(t, TLSSecretName).Data[corev1.TLSCertKey]; !bytes.Equal(got, issued) {
		t.Error("the ACME certificate is not served once issued")
	}

	// Leaving ACME removes what cert-manager would otherwise keep renewing.
	a := getAppliance(t)
	a.Spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMECertificateName}, certManagerObject("Certificate")); !apierrors.IsNotFound(err) {
		t.Errorf("ACME Certificate after leaving ACME: %v", err)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMESecretName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("ACME Secret after leaving ACME: %v", err)
	}
}

func TestReconcileACMEWaitsForILM(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node: v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		TLS: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: &v1alpha1.ACMESpec{
			ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: "ra", VaultUUID: "v", VaultProfileUUID: "vp"},
		}},
	})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForILM {
		t.Errorf("CertificateReady = %+v, want False waiting for ILM", c)
	}
	leafCertificate(t) // the built-in certificate is served meanwhile
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMECertificateName}, certManagerObject("Certificate")); err == nil {
		t.Error("the ACME certificate was requested before ILM provides the directory")
	}
}

// eabKey is the HMAC key the fake ILM generates; it must never reach a condition.
const eabKey = "ZWFiLWtleS1vZi1pbG0"

// fakeACMEILM is the ILM edge with the endpoints of the ILM preset: vault profiles, EAB keys, secrets and one ACME
// profile, which it keeps.
type fakeACMEILM struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []string
	// vaults are the vault profiles ILM lists.
	vaults []map[string]any
	// profile is the ACME profile as last written, nil until created.
	profile map[string]any
	enabled bool
	secrets int
	down    bool
	// refuseProfile answers a new ACME profile with 400.
	refuseProfile bool
	// secretsGone answers 404 for every secret, as for one deleted in ILM.
	secretsGone bool
	// slow keeps every request open until the client gives up or the test ends.
	slow atomic.Bool
	stop chan struct{}
}

func newFakeACMEILM(t *testing.T) *fakeACMEILM {
	t.Helper()
	f := &fakeACMEILM{stop: make(chan struct{}), vaults: []map[string]any{
		{"uuid": "vp1", "name": "hashicorp", "enabled": true, "vaultInstance": map[string]any{"uuid": "v1"}},
	}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.stop) })
	return f
}

func (f *fakeACMEILM) serve(w http.ResponseWriter, r *http.Request) {
	if f.slow.Load() {
		select {
		case <-r.Context().Done():
		case <-f.stop:
		}
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, call)
	if f.down {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	answer := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if strings.HasPrefix(call, "GET /api/v1/secrets/") && !f.secretsGone {
		answer(map[string]string{"uuid": strings.TrimPrefix(call, "GET /api/v1/secrets/")})
		return
	}
	switch call {
	case "POST /api/v1/vaultProfiles/list":
		answer(map[string]any{"items": f.vaults})
	case "POST /api/v1/acmeProfiles/eabKeys":
		answer(map[string]string{"key": eabKey})
	case "POST /api/v1/vaults/v1/vaultProfiles/vp1/secrets", "POST /api/v1/vaults/v2/vaultProfiles/vp2/secrets":
		f.secrets++
		f.requests[len(f.requests)-1] += " " + body["name"].(string) + " " + body["secret"].(map[string]any)["content"].(string)
		answer(map[string]string{"uuid": fmt.Sprintf("secret-%d", f.secrets)})
	case "GET /api/v1/acmeProfiles":
		if f.profile == nil {
			answer([]any{})
			return
		}
		answer([]map[string]any{{"uuid": "a1", "name": f.profile["name"], "enabled": f.enabled}})
	case "POST /api/v1/acmeProfiles":
		if f.refuseProfile {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.profile = body
		answer(map[string]string{"uuid": "a1"})
	case "PUT /api/v1/acmeProfiles/a1":
		body["name"] = f.profile["name"]
		f.profile = body
		answer(map[string]string{"uuid": "a1"})
	case "GET /api/v1/acmeProfiles/a1":
		answer(map[string]any{
			"uuid": "a1", "name": f.profile["name"], "enabled": f.enabled,
			"raProfile": map[string]any{"uuid": f.profile["raProfileUuid"]}, "eabSecretUuids": f.profile["eabSecretUuids"],
			"preauthorizedIdentifiers": f.profile["preauthorizedIdentifiers"], "identifierAuthorizationMode": f.profile["identifierAuthorizationMode"],
		})
	case "PATCH /api/v1/acmeProfiles/a1/enable":
		f.enabled = true
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// writes are the calls that change ILM.
func (f *fakeACMEILM) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.requests {
		if !strings.HasPrefix(c, http.MethodGet) && !strings.HasPrefix(c, "POST /api/v1/vaultProfiles/list") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeACMEILM) identifiers() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profile["preauthorizedIdentifiers"].([]any)
}

// edgeIdentity returns a client of a fake ILM edge, or err.
type edgeIdentity struct {
	srv   *httptest.Server
	err   error
	calls int
}

func (e *edgeIdentity) Ensure(context.Context, string, string, *x509.CertPool) (*ilmclient.Client, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	pool := x509.NewCertPool()
	pool.AddCert(e.srv.Certificate())
	return ilmclient.New(e.srv.Listener.Addr().String(), "example.com", tls.Certificate{}, pool), nil
}

// presetFixture returns a reconciler whose ILM is ilm and an Appliance choosing the ILM preset, with ILM ready.
func presetFixture(t *testing.T, ilm *fakeACMEILM) (*ApplianceReconciler, *edgeIdentity, *v1alpha1.Appliance) {
	t.Helper()
	cleanCertificates(t)
	ctx := context.Background()
	spec := v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		Platform: &v1alpha1.PlatformSpec{HostName: "pki.example.com"},
		TLS: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: &v1alpha1.ACMESpec{
			Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01},
			ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: "ra1", VaultUUID: "v1", VaultProfileUUID: "vp1",
				ProfileName: "appliance"},
		}},
	}
	createAppliance(t, spec)
	tlsCopy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.TLSSecretName},
		Data:       map[string][]byte{corev1.TLSCertKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ilm.srv.Certificate().Raw})},
	}
	if err := testClient.Create(ctx, tlsCopy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = testClient.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: acmeNetworkPolicyName}})
	})
	identity := &edgeIdentity{srv: ilm.srv}
	r := &ApplianceReconciler{Client: testClient, APIReader: testClient, Release: testRelease, ILM: identity}
	a := getAppliance(t)
	setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionTrue, reasonReady, "ready")
	setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionTrue, reasonApplied, "the ILM settings are applied")
	return r, identity, a
}

func certificateReady(t *testing.T, a *v1alpha1.Appliance) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionCertificateReady)
	if c == nil {
		t.Fatal("no CertificateReady condition")
	}
	if strings.Contains(c.Message, eabKey) {
		t.Fatalf("CertificateReady quotes the EAB key: %q", c.Message)
	}
	return *c
}

func TestILMPresetWaitsWhileILMDoesNotAnswer(t *testing.T) {
	ilm := newFakeACMEILM(t)
	r, identity, a := presetFixture(t, ilm)
	ctx := context.Background()

	ready := a.DeepCopy()
	meta.RemoveStatusCondition(&a.Status.Conditions, v1alpha1.ConditionILMSettingsApplied)
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if c := certificateReady(t, a); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForILM {
		t.Errorf("CertificateReady = %+v, want waiting for ILM while its settings are not applied", c)
	}
	if identity.calls != 0 {
		t.Errorf("asked for the ILM identity %d times before ILM is ready", identity.calls)
	}

	for name, breakILM := range map[string]func(){
		"down":           func() { ilm.down = true },
		"not registered": func() { identity.err = errors.New("the identity is not registered") },
	} {
		t.Run(name, func(t *testing.T) {
			a := ready.DeepCopy()
			breakILM()
			r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
			if c := certificateReady(t, a); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForILM {
				t.Errorf("CertificateReady = %+v, want waiting for ILM", c)
			}
			leafCertificate(t) // the built-in certificate is served meanwhile
			if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMECertificateName}, certManagerObject("Certificate")); err == nil {
				t.Error("the ACME certificate was requested without ILM")
			}
		})
	}
}

func TestILMPresetWaitsWithinTheILMPassTimeout(t *testing.T) {
	ilm := newFakeACMEILM(t)
	r, _, a := presetFixture(t, ilm)
	ilm.slow.Store(true)
	timeout := ilmPassTimeout
	ilmPassTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ilmPassTimeout = timeout })

	start := time.Now()
	r.reconcileCertificate(context.Background(), a, addons.Progress{Done: true})
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the certificate step took %v with a slow ILM", took)
	}
	if c := certificateReady(t, a); c.Reason != reasonWaitingForILM {
		t.Errorf("CertificateReady = %+v, want waiting for ILM", c)
	}
}

func TestStaleSourceKeepsTheACMECredentialsTheAPIJustWrote(t *testing.T) {
	ctx := context.Background()
	names := []string{certs.ACMEEABSecretName, certs.ACMEDNS01SecretName}
	for _, name := range names {
		if err := testClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: name}, Data: map[string][]byte{"k": []byte("x")},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = testClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: name}})
		})
	}
	// The API switched to acme, but this pass still reads the Appliance with the earlier source.
	stale := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{TLS: v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin}}}
	r := &ApplianceReconciler{Client: testClient, APIReader: testClient, Release: testRelease}

	r.reconcileCertificate(ctx, stale, addons.Progress{})
	for _, name := range names {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: name}, &corev1.Secret{}); err != nil {
			t.Errorf("Secret %s after the pass: %v", name, err)
		}
	}
}

func TestILMPresetSetsUpILMOnce(t *testing.T) {
	ilm := newFakeACMEILM(t)
	r, _, a := presetFixture(t, ilm)
	ctx := context.Background()
	// A custom directory's binding, which the preset leaves alone.
	if err := testClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.ACMEEABSecretName},
		Data:       map[string][]byte{certs.ACMEEABHMACKey: []byte("custom")},
	}); err != nil {
		t.Fatal(err)
	}

	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if c := certificateReady(t, a); c.Status != metav1.ConditionFalse || c.Reason != reasonIssuing {
		t.Errorf("CertificateReady = %+v, want issuing", c)
	}
	writes := ilm.writes()
	if len(writes) != 4 || writes[0] != "POST /api/v1/acmeProfiles/eabKeys" ||
		!strings.HasPrefix(writes[1], "POST /api/v1/vaults/v1/vaultProfiles/vp1/secrets "+certs.ACMEILMEABSecretName+"-") ||
		!strings.HasSuffix(writes[1], " "+eabKey) ||
		writes[2] != "POST /api/v1/acmeProfiles" || writes[3] != "PATCH /api/v1/acmeProfiles/a1/enable" {
		t.Fatalf("writes = %q", writes)
	}
	wantIdentifiers := []any{
		map[string]any{"type": "dns", "value": "appliance.example.com", "matchType": "exact"},
		map[string]any{"type": "dns", "value": "pki.example.com", "matchType": "exact"},
		map[string]any{"type": "ip", "value": "192.0.2.10", "matchType": "exact"},
	}
	if got := ilm.identifiers(); !reflect.DeepEqual(got, wantIdentifiers) {
		t.Errorf("identifiers %v, want %v", got, wantIdentifiers)
	}
	if ilm.profile["raProfileUuid"] != "ra1" || !reflect.DeepEqual(ilm.profile["eabSecretUuids"], []any{"secret-1"}) {
		t.Errorf("profile %v", ilm.profile)
	}
	eab := &corev1.Secret{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMEILMEABSecretName}, eab); err != nil {
		t.Fatal(err)
	}
	if string(eab.Data[certs.ACMEEABHMACKey]) != eabKey || string(eab.Data[certs.ACMEEABKeyIDKey]) != "secret-1" {
		t.Errorf("EAB Secret keys %v", slices.Collect(maps.Keys(eab.Data)))
	}
	custom := &corev1.Secret{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMEEABSecretName}, custom); err != nil {
		t.Fatal(err)
	}
	if len(custom.Data) != 1 || string(custom.Data[certs.ACMEEABHMACKey]) != "custom" {
		t.Errorf("the custom directory's EAB Secret was changed: keys %v", slices.Collect(maps.Keys(custom.Data)))
	}

	issuer := certManagerObject("Issuer")
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMEIssuerName}, issuer); err != nil {
		t.Fatal(err)
	}
	acme, _, _ := unstructured.NestedMap(issuer.Object, "spec", "acme")
	if want := platform.CoreAPIURL + "/v1/protocols/acme/appliance/directory"; acme["server"] != want {
		t.Errorf("server %v, want %s", acme["server"], want)
	}
	if kid, _, _ := unstructured.NestedString(acme, "externalAccountBinding", "keyID"); kid != "secret-1" {
		t.Errorf("EAB key ID %q, want the ILM secret", kid)
	}
	if ref, _, _ := unstructured.NestedString(acme, "externalAccountBinding", "keySecretRef", "name"); ref != certs.ACMEILMEABSecretName {
		t.Errorf("EAB Secret %q, want the preset's", ref)
	}
	if _, found := acme["caBundle"]; found {
		t.Error("a CA bundle is set for the in-cluster directory")
	}
	if ips, _, _ := unstructured.NestedStringSlice(acmeCertificate(t).Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"192.0.2.10"}) {
		t.Errorf("ipAddresses %v, want the node address", ips)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: acmeNetworkPolicyName}, np); err != nil {
		t.Fatalf("network policy: %v", err)
	}
	if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] != "core" || len(np.Spec.Ingress) != 1 ||
		np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "cert-manager" ||
		np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "controller" ||
		np.Spec.Ingress[0].Ports[0].Port.IntValue() != 8080 {
		t.Errorf("network policy %+v", np.Spec)
	}

	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if got := ilm.writes(); len(got) != 4 {
		t.Errorf("writes after the second pass %q, want none new", got[4:])
	}

	// A new node address and host name update the profile's identifiers, with the same key.
	a.Spec.Node.Address = "192.0.2.11/24"
	a.Spec.Platform.HostName = "ilm.example.com"
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if got := ilm.writes(); len(got) != 5 || got[4] != "PUT /api/v1/acmeProfiles/a1" {
		t.Errorf("writes %q, want the profile updated", got)
	}
	ids := ilm.identifiers()
	if len(ids) != 3 || ids[1].(map[string]any)["value"] != "ilm.example.com" || ids[2].(map[string]any)["value"] != "192.0.2.11" {
		t.Errorf("identifiers %v", ids)
	}

	// Away from the preset, cert-manager no longer reaches Core.
	a.Spec.TLS.ACME = &v1alpha1.ACMESpec{DirectoryURL: acmeDirectory, Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01}}
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(np), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Errorf("network policy after leaving the preset: %v", err)
	}
}

func TestILMPresetStoresANewKeyInAnotherVaultProfile(t *testing.T) {
	ilm := newFakeACMEILM(t)
	ilm.vaults = append(ilm.vaults, map[string]any{"uuid": "vp2", "name": "other", "enabled": true, "vaultInstance": map[string]any{"uuid": "v2"}})
	r, _, a := presetFixture(t, ilm)
	ctx := context.Background()
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})

	a.Spec.TLS.ACME.ILMProfile.VaultUUID, a.Spec.TLS.ACME.ILMProfile.VaultProfileUUID = "v2", "vp2"
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if !slices.ContainsFunc(ilm.writes(), func(w string) bool {
		return strings.HasPrefix(w, "POST /api/v1/vaults/v2/vaultProfiles/vp2/secrets ")
	}) {
		t.Errorf("writes %q, want a new key in the chosen vault profile", ilm.writes())
	}
	if !reflect.DeepEqual(ilm.profile["eabSecretUuids"], []any{"secret-2"}) {
		t.Errorf("profile %v, want the new key", ilm.profile)
	}
}

func TestILMPresetNeedsAVaultProfile(t *testing.T) {
	ilm := newFakeACMEILM(t)
	ilm.vaults = nil
	r, _, a := presetFixture(t, ilm)

	r.reconcileCertificate(context.Background(), a, addons.Progress{Done: true})
	if c := certificateReady(t, a); c.Status != metav1.ConditionFalse || c.Message != "ILM needs a vault profile to hold the ACME account key" {
		t.Errorf("CertificateReady = %+v", c)
	}
	if got := ilm.writes(); len(got) != 0 {
		t.Errorf("writes %q, want none", got)
	}
}

func TestILMPresetReportsARefusedProfile(t *testing.T) {
	ilm := newFakeACMEILM(t)
	ilm.refuseProfile = true
	r, _, a := presetFixture(t, ilm)
	ctx := context.Background()

	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if c := certificateReady(t, a); c.Status != metav1.ConditionFalse || c.Reason != reasonFailed || c.Message != messageProfileRefused {
		t.Errorf("CertificateReady = %+v, want the refused profile", c)
	}
	// The key stored in ILM is kept for the next attempt.
	eab := &corev1.Secret{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMEILMEABSecretName}, eab); err != nil {
		t.Fatal(err)
	}
	if string(eab.Data[certs.ACMEEABKeyIDKey]) != "secret-1" {
		t.Errorf("EAB key ID %q", eab.Data[certs.ACMEEABKeyIDKey])
	}
	ilm.refuseProfile = false
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if ilm.secrets != 1 {
		t.Errorf("%d secrets created in ILM, want 1", ilm.secrets)
	}
}

func TestILMPresetReplacesAKeyGoneFromILM(t *testing.T) {
	ilm := newFakeACMEILM(t)
	r, _, a := presetFixture(t, ilm)
	ctx := context.Background()
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})

	ilm.secretsGone = true
	r.reconcileCertificate(ctx, a, addons.Progress{Done: true})
	if c := certificateReady(t, a); c.Reason != reasonIssuing {
		t.Errorf("CertificateReady = %+v, want issuing with a new key", c)
	}
	if ilm.secrets != 2 || !reflect.DeepEqual(ilm.profile["eabSecretUuids"], []any{"secret-2"}) {
		t.Errorf("%d secrets created, profile %v; want the profile on a new key", ilm.secrets, ilm.profile)
	}
	eab := &corev1.Secret{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMEILMEABSecretName}, eab); err != nil {
		t.Fatal(err)
	}
	if string(eab.Data[certs.ACMEEABKeyIDKey]) != "secret-2" {
		t.Errorf("EAB key ID %q, want the new key", eab.Data[certs.ACMEEABKeyIDKey])
	}
}

func TestILMPresetRunsWithTheManagerRBAC(t *testing.T) {
	ctx := context.Background()
	c := managerClient(t)
	if err := applyACMENetworkPolicy(ctx, c); err != nil {
		t.Fatalf("apply the network policy as the manager: %v", err)
	}
	if err := removeACMENetworkPolicy(ctx, c); err != nil {
		t.Errorf("remove the network policy as the manager: %v", err)
	}
}
