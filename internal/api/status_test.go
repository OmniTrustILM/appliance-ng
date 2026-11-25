package api

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
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

func TestStatus(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupPart2)
	changed := metav1.NewTime(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))
	a.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionAddonsReady, Status: metav1.ConditionFalse, Reason: "Progressing", Message: "traefik: waiting for Deployment traefik/traefik: 0/1 ready", LastTransitionTime: changed},
		{Type: v1alpha1.ConditionCertificateReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: "the built-in certificate is issued", LastTransitionTime: changed},
	}
	a.Status.Versions = v1alpha1.Versions{Appliance: "0.2.0", ILM: "2.20.0"}
	a.Status.NodeAddress = "192.0.2.5"
	a.Status.DataVolume = &v1alpha1.VolumeStatus{SizeBytes: 100, FreeBytes: 40, LowSpace: true}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	rec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Status
	decode(t, rec, &got)
	if got.Setup != StatusSetupPart2 || got.AddonStep == nil || *got.AddonStep != "traefik" || got.RebootRequired {
		t.Errorf("status = %+v, want part2 at add-on step traefik", got)
	}
	if len(got.Conditions) != 2 || got.Conditions[0].Type != v1alpha1.ConditionAddonsReady || *got.Conditions[0].Reason != "Progressing" ||
		got.Conditions[1].Status != True || !got.Conditions[1].LastTransitionTime.Equal(changed.Time) {
		t.Errorf("conditions = %+v", got.Conditions)
	}
	if got.Versions.Appliance == nil || *got.Versions.Appliance != "0.2.0" || got.Versions.Ilm == nil || got.Versions.Talos != nil {
		t.Errorf("versions = %+v", got.Versions)
	}
	if got.NodeAddress == nil || *got.NodeAddress != "192.0.2.5" {
		t.Errorf("node address = %v", got.NodeAddress)
	}
	if got.DataVolume == nil || got.DataVolume.SizeBytes != 100 || got.DataVolume.FreeBytes != 40 || !got.DataVolume.LowSpace {
		t.Errorf("data volume = %+v", got.DataVolume)
	}
}

func TestStatusCertificate(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupComplete)
	a.Status.RebootRequired = true
	a.Status.Versions = v1alpha1.Versions{Appliance: "0.2.0", Talos: "v1.14.5", Kubernetes: "v1.37.1", Operator: "v1.1.0", ILM: "2.20.0"}
	a.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionConnectorsReady, Status: metav1.ConditionFalse, Reason: "Failed", Message: "these connectors failed: ejbca-ng-connector", LastTransitionTime: metav1.Now()}}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	status := func() Status {
		var got Status
		rec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
		wantStatus(t, rec, http.StatusOK)
		decode(t, rec, &got)
		return got
	}
	if got := status(); got.Certificate != nil {
		t.Errorf("certificate = %+v before one is issued, want none", got.Certificate)
	}

	ca := newTestCA(t)
	builtin, _ := ca.leaf(t)
	createTLSSecret(t, certs.SecretName, builtin)
	got := status()
	wantNotAfter := certIssued.Add(90 * 24 * time.Hour)
	if got.Certificate == nil || !got.Certificate.NotAfter.Equal(wantNotAfter) {
		t.Errorf("certificate = %+v, want notAfter %v", got.Certificate, wantNotAfter)
	}
	if !got.RebootRequired || got.Versions.Talos == nil || *got.Versions.Talos != "v1.14.5" || got.Versions.Operator == nil || *got.Versions.Operator != "v1.1.0" {
		t.Errorf("status = %+v, want the reboot and all versions", got)
	}
	if len(got.Conditions) != 1 || got.Conditions[0].Type != v1alpha1.ConditionConnectorsReady {
		t.Errorf("conditions = %+v, want ConnectorsReady", got.Conditions)
	}

	// An uploaded certificate in use replaces the built-in one.
	key := newECKey(t)
	uploaded := ca.issue(t, key.Public(), []string{"appliance.example.com"}, nil)
	createTLSSecret(t, "custom-tls", uploaded)
	a = getAppliance(t)
	a.Spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "custom-tls"}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(uploaded)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(); got.Certificate == nil || !got.Certificate.NotAfter.Equal(leaf.NotAfter) {
		t.Errorf("certificate = %+v, want the uploaded one's notAfter %v", got.Certificate, leaf.NotAfter)
	}
}

// createTLSSecret creates the TLS Secret name in the system namespace with the certificate certPEM.
func createTLSSecret(t *testing.T, name string, certPEM []byte) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: []byte("key")},
	}
	if err := testClient.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
}

func TestStatusOnceAddonsAreReady(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupComplete)
	a.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionAddonsReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: "all add-ons are ready", LastTransitionTime: metav1.Now()}}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var got Status
	decode(t, h.request(http.MethodGet, "/appliance/api/v1/status", nil, s), &got)
	if got.Setup != StatusSetupComplete || got.AddonStep != nil {
		t.Errorf("status = %+v, want complete without an add-on step", got)
	}
}

func TestStatusBeforeTheManagerReconciled(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, "")
	var got Status
	decode(t, h.request(http.MethodGet, "/appliance/api/v1/status", nil, s), &got)
	if got.Setup != StatusSetupPart2 || got.Conditions == nil {
		t.Errorf("status = %+v, want part2 with an empty condition list", got)
	}
}

func TestGetAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupPart2)
	a.Spec.Node.Address = "192.0.2.5/24"
	a.Spec.Node.Gateway = "192.0.2.1"
	a.Spec.DNS.Servers = []string{"192.0.2.53"}
	a.Spec.NTP.Servers = []string{"time.example.com"}
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTPS: "http://proxy.example.com:3128"}
	a.Spec.Trust = &v1alpha1.SecretKeyRef{Name: "trust", Key: "ca.crt"}
	a.Spec.Registries = []v1alpha1.RegistrySpec{{Host: "registry.example.com", CredentialsRef: "registry"}, {Host: "mirror.example.com"}}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}, Mail: validMail()}, s), http.StatusAccepted)

	rec := h.request(http.MethodGet, "/appliance/api/v1/appliance", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got ApplianceSettings
	decode(t, rec, &got)
	if got.Node.HostName != "appliance.example.com" || *got.Node.Address != "192.0.2.5/24" || *got.Node.Gateway != "192.0.2.1" {
		t.Errorf("node = %+v", got.Node)
	}
	if got.TlsSource != ApplianceSettingsTlsSourceBuiltin || !got.TrustSet || len(got.Dns) != 1 || len(got.Ntp) != 1 || got.Proxy == nil || *got.Proxy.Https == "" {
		t.Errorf("settings = %+v", got)
	}
	if len(got.Registries) != 2 || !got.Registries[0].CredentialsSet || got.Registries[1].CredentialsSet {
		t.Errorf("registries = %+v", got.Registries)
	}
	if m := got.Mail; m == nil || !m.PasswordSet || m.Username == nil || *m.Username != "mailer" || m.Port != 587 || m.Security != Starttls {
		t.Errorf("mail = %+v", got.Mail)
	}
	if p := got.Platform; p == nil || *p.HostName != "ilm.example.com" || *p.IlmVersion != "2.20.0" || len(p.Components) != 1 {
		t.Errorf("platform = %+v", got.Platform)
	}
}

func TestGetApplianceWithoutOptionalSettings(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	rec := h.request(http.MethodGet, "/appliance/api/v1/appliance", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got ApplianceSettings
	decode(t, rec, &got)
	if got.Dns == nil || got.Ntp == nil || got.Registries == nil || got.Mail != nil || got.Platform != nil || got.Proxy != nil || got.TrustSet {
		t.Errorf("settings = %+v, want empty lists and no optional settings", got)
	}
}

func TestCaPem(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.request(http.MethodGet, "/appliance/api/v1/ca.pem", nil, nil), http.StatusNotFound)

	root := selfSignedPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.SecretName},
		Data:       map[string][]byte{"ca.crt": root},
	}
	if err := testClient.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	rec := h.request(http.MethodGet, "/appliance/api/v1/ca.pem", nil, nil)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "application/x-pem-file" {
		t.Errorf("Content-Type %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got == "" {
		t.Error("no Content-Disposition")
	}
	if rec.Body.String() != string(root) {
		t.Errorf("body = %q, want the root certificate", rec.Body)
	}
}

func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// failingGet refuses to read the object name.
type failingGet struct {
	client.Client
	name string
}

func (c failingGet) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == c.name {
		return errors.New("read refused")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestStatusSurvivesACertificateReadError(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	builtin, _ := newTestCA(t).leaf(t)
	createTLSSecret(t, certs.SecretName, builtin)
	status := func() Status {
		var got Status
		rec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
		wantStatus(t, rec, http.StatusOK)
		decode(t, rec, &got)
		return got
	}

	// The status reads the certificate through the cached client, not past the cache.
	h.srv.deps.Reader = failingGet{Client: testClient, name: certs.SecretName}
	if got := status(); got.Certificate == nil {
		t.Error("no certificate read through the cached client")
	}

	h.srv.deps.Client = failingGet{Client: testClient, name: certs.SecretName}
	if got := status(); got.Certificate != nil || got.Setup != StatusSetupComplete {
		t.Errorf("status %+v, want it without the certificate", got)
	}
	if !strings.Contains(h.logs.String(), "certificate in use not read") {
		t.Errorf("log %s, want the read error", h.logs)
	}
}

func TestStatusSurvivesARecoveryKitReadError(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	h.srv.deps.Reader = failingGet{Client: testClient, name: recovery.RecipientSecret}

	rec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Status
	decode(t, rec, &got)
	if got.RecoveryKit != nil || got.Setup != StatusSetupComplete {
		t.Errorf("status %+v, want it without the recovery kit", got)
	}
	if strings.Contains(rec.Body.String(), "recoveryKit") {
		t.Errorf("body %s, want no recoveryKit", rec.Body)
	}
	if !strings.Contains(h.logs.String(), "recovery kit not read") {
		t.Errorf("log %s, want the read error", h.logs)
	}
}

func TestStatusReportsRestore(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupRestore)
	rec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Status
	decode(t, rec, &got)
	if got.Setup != StatusSetupRestore {
		t.Errorf("setup %q, want restore", got.Setup)
	}
}

func TestReadiness(t *testing.T) {
	h := newHarness(t)
	get := func() bool {
		t.Helper()
		rec := h.request(http.MethodGet, "/appliance/api/v1/readiness", nil, nil)
		wantStatus(t, rec, http.StatusOK)
		var got Readiness
		decode(t, rec, &got)
		return got.SignIn
	}
	if got := get(); got {
		t.Error("signIn true without an Appliance")
	}

	a := createAppliance(t, v1alpha1.SetupPart2)
	if got := get(); got {
		t.Error("signIn true before the hand-over")
	}

	a.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionHandoverComplete, Status: metav1.ConditionTrue, Reason: "Done", LastTransitionTime: metav1.Now()}}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := get(); !got {
		t.Error("signIn false after the hand-over")
	}
	if entries := h.entries(); len(entries) != 0 {
		t.Errorf("readiness wrote %d audit entries", len(entries))
	}
}
