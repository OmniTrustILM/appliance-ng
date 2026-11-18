package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
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
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	rec := h.request(http.MethodGet, "/api/v1/status", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Status
	decode(t, rec, &got)
	if got.Setup != Part2 || got.AddonStep == nil || *got.AddonStep != "traefik" || got.RebootRequired {
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
	decode(t, h.request(http.MethodGet, "/api/v1/status", nil, s), &got)
	if got.Setup != Complete || got.AddonStep != nil {
		t.Errorf("status = %+v, want complete without an add-on step", got)
	}
}

func TestStatusBeforeTheManagerReconciled(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, "")
	var got Status
	decode(t, h.request(http.MethodGet, "/api/v1/status", nil, s), &got)
	if got.Setup != Part2 || got.Conditions == nil {
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
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}, Mail: validMail()}, s), http.StatusAccepted)

	rec := h.request(http.MethodGet, "/api/v1/appliance", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got ApplianceSettings
	decode(t, rec, &got)
	if got.Node.HostName != "appliance.example.com" || *got.Node.Address != "192.0.2.5/24" || *got.Node.Gateway != "192.0.2.1" {
		t.Errorf("node = %+v", got.Node)
	}
	if got.TlsSource != Builtin || !got.TrustSet || len(got.Dns) != 1 || len(got.Ntp) != 1 || got.Proxy == nil || *got.Proxy.Https == "" {
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
	rec := h.request(http.MethodGet, "/api/v1/appliance", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got ApplianceSettings
	decode(t, rec, &got)
	if got.Dns == nil || got.Ntp == nil || got.Registries == nil || got.Mail != nil || got.Platform != nil || got.Proxy != nil || got.TrustSet {
		t.Errorf("settings = %+v, want empty lists and no optional settings", got)
	}
}

func TestCaPem(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.request(http.MethodGet, "/api/v1/ca.pem", nil, nil), http.StatusNotFound)

	root := selfSignedPEM(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.SecretName},
		Data:       map[string][]byte{"ca.crt": root},
	}
	if err := testClient.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	rec := h.request(http.MethodGet, "/api/v1/ca.pem", nil, nil)
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
