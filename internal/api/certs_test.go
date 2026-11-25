package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
)

const (
	certNodeAddress = "192.0.2.10"
	pathCertificate = "/appliance/api/v1/certificate"
	pathCSR         = "/appliance/api/v1/certificate/csr"
	pathSigned      = "/appliance/api/v1/certificate/csr/signed"
	pathBuiltin     = "/appliance/api/v1/certificate/builtin"
	pathACME        = "/appliance/api/v1/certificate/acme"
)

// certIssued is when the test certificates become valid, the day before the harness clock.
var certIssued = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

// testCA is a root CA that issues the test certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key := newECKey(t)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: certIssued.Add(-24 * time.Hour), NotAfter: certIssued.Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a server certificate in PEM for pub with the given names.
func (ca *testCA) issue(t *testing.T, pub crypto.PublicKey, hosts []string, ips []net.IP) []byte {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: hosts[0]},
		DNSNames: hosts, IPAddresses: ips,
		NotBefore: certIssued, NotAfter: certIssued.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// leaf returns a certificate for the test Appliance's names and address and its key, both in PEM.
func (ca *testCA) leaf(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key := newECKey(t)
	return ca.issue(t, key.Public(), []string{"appliance.example.com"}, []net.IP{net.ParseIP(certNodeAddress)}), pkcs8PEM(t, key)
}

// sign issues the certificate a signing request asks for.
func (ca *testCA) sign(t *testing.T, csrPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatalf("not a PEM signing request: %q", csrPEM)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return ca.issue(t, csr.PublicKey, csr.DNSNames, csr.IPAddresses)
}

func pkcs8PEM(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// createCertificateAppliance creates the Appliance after setup with the node address in its status.
func createCertificateAppliance(t *testing.T) {
	t.Helper()
	a := createAppliance(t, v1alpha1.SetupComplete)
	a.Status.NodeAddress = certNodeAddress
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// upload sends a multipart upload with the given parts to the handler.
func (h *harness) upload(method, path string, parts map[string][]byte, s *session) *httptest.ResponseRecorder {
	h.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, data := range parts {
		part, err := w.CreateFormFile(name, name+".pem")
		if err != nil {
			h.t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		h.t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, &body)
	req.RemoteAddr = testPeer
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.cookie})
	req.Header.Set(auth.CSRFHeader, s.csrf)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func systemSecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: name}, secret); err != nil {
		t.Fatalf("secret %s: %v", name, err)
	}
	return secret
}

func secretGone(t *testing.T, name string) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: name}, &corev1.Secret{})
	return client.IgnoreNotFound(err) == nil && err != nil
}

func (h *harness) certificateInfo(s *session) CertificateInfo {
	h.t.Helper()
	rec := h.request(http.MethodGet, pathCertificate, nil, s)
	wantStatus(h.t, rec, http.StatusOK)
	var info CertificateInfo
	decode(h.t, rec, &info)
	return info
}

func wantMessage(t *testing.T, rec *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	wantStatus(t, rec, status)
	var e Error
	decode(t, rec, &e)
	if e.Message != message {
		t.Errorf("message %q, want %q", e.Message, message)
	}
}

func TestUploadAccepts(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	certPEM, keyPEM := ca.leaf(t)

	rec := h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": keyPEM, "chain": ca.pem}, s)
	wantStatus(t, rec, http.StatusAccepted)

	secret := systemSecret(t, certs.CustomSecretName)
	if secret.Type != corev1.SecretTypeTLS || string(secret.Data[corev1.TLSPrivateKeyKey]) != string(keyPEM) {
		t.Errorf("secret of type %s with another key", secret.Type)
	}
	if want := string(bytes.TrimSpace(certPEM)) + "\n" + string(ca.pem); string(secret.Data[corev1.TLSCertKey]) != want {
		t.Errorf("tls.crt is not the certificate followed by the chain:\n%s", secret.Data[corev1.TLSCertKey])
	}
	if got := getAppliance(t).Spec.TLS; got != (v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: certs.CustomSecretName}) {
		t.Errorf("tls %+v", got)
	}
	entries := h.entries()
	last := entries[len(entries)-1]
	if last.Action != string(AuditEntryActionCertificateUpload) || last.Result != string(Success) ||
		!slices.Equal(last.Paths, []string{"spec.tls.source", "spec.tls.secretRef"}) {
		t.Errorf("audit entry %+v", last)
	}

	info := h.certificateInfo(s)
	if info.Source != CertificateInfoSourceSecret || info.CsrPending || info.Certificate == nil {
		t.Fatalf("info %+v", info)
	}
	d := info.Certificate
	if d.Subject != "CN=appliance.example.com" || d.Issuer != "CN=Test CA" ||
		!slices.Equal(d.DnsNames, []string{"appliance.example.com"}) || !slices.Equal(d.IpAddresses, []string{certNodeAddress}) {
		t.Errorf("details %+v", d)
	}

	// The same upload again changes no Appliance field.
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": keyPEM, "chain": ca.pem}, s), http.StatusAccepted)
	entries = h.entries()
	if last := entries[len(entries)-1]; len(last.Paths) != 0 {
		t.Errorf("paths %v for an unchanged source", last.Paths)
	}
}

func TestUploadChainsToTheTrustedCAs(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	// A certificate of a public CA names the host only, not the node's private address.
	key := newECKey(t)
	certPEM := ca.issue(t, key.Public(), []string{"appliance.example.com"}, nil)
	upload := map[string][]byte{"certificate": certPEM, "key": pkcs8PEM(t, key)}

	wantMessage(t, h.upload(http.MethodPut, pathCertificate, upload, s), http.StatusBadRequest,
		"The chain does not lead to a trusted root certificate; add the intermediate and root certificates.")
	wantStatus(t, h.request(http.MethodPost, pathTrust, TrustUpload{Pem: string(ca.pem)}, s), http.StatusAccepted)
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, upload, s), http.StatusAccepted)
}

func TestFailedUploadKeepsCurrent(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	certPEM, keyPEM := ca.leaf(t)

	// From the built-in certificate: nothing is written.
	rec := h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": pkcs8PEM(t, newECKey(t)), "chain": ca.pem}, s)
	wantMessage(t, rec, http.StatusBadRequest, "The private key does not match the certificate.")
	if !secretGone(t, certs.CustomSecretName) || getAppliance(t).Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		t.Error("a refused upload was installed")
	}

	// From an uploaded certificate: it stays.
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": keyPEM, "chain": ca.pem}, s), http.StatusAccepted)
	other := newECKey(t)
	wrongName := ca.issue(t, other.Public(), []string{"other.example.com"}, []net.IP{net.ParseIP(certNodeAddress)})
	rec = h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": wrongName, "key": pkcs8PEM(t, other), "chain": ca.pem}, s)
	wantMessage(t, rec, http.StatusBadRequest, "The certificate does not cover the host name appliance.example.com.")
	if got := systemSecret(t, certs.CustomSecretName).Data[corev1.TLSPrivateKeyKey]; string(got) != string(keyPEM) {
		t.Error("a refused upload replaced the certificate")
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Result != string(Failure) || len(last.Paths) != 0 {
		t.Errorf("audit entry %+v", last)
	}
	if strings.Contains(h.logs.String(), keyBody(pkcs8PEM(t, other))) {
		t.Error("the log holds the refused key")
	}
}

func TestUploadNeedsItsParts(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	certPEM, keyPEM := ca.leaf(t)

	rec := h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	var e Error
	decode(t, rec, &e)
	if e.Field == nil || *e.Field != "key" {
		t.Errorf("error %+v, want field key", e)
	}
	wantStatus(t, h.request(http.MethodPut, pathCertificate, `{"certificate":"x"}`, s), http.StatusBadRequest)
	big := map[string][]byte{"certificate": certPEM, "key": keyPEM, "padding": bytes.Repeat([]byte("a"), maxUploadBody)}
	wantMessage(t, h.upload(http.MethodPut, pathCertificate, big, s), http.StatusBadRequest, messageUnreadable)

	// More than the 16 KiB of other requests, with an unknown part, is accepted.
	padded := map[string][]byte{"certificate": certPEM, "key": keyPEM, "chain": ca.pem, "padding": bytes.Repeat([]byte("a"), 2*maxBody)}
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, padded, s), http.StatusAccepted)
}

func TestCSRKeyStaysInCluster(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)

	rec := h.request(http.MethodPost, pathCSR, nil, s)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "application/x-pem-file" {
		t.Errorf("Content-Type %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "ilm-appliance.csr") {
		t.Errorf("Content-Disposition %q", got)
	}
	body := rec.Body.Bytes()
	block, rest := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("the answer is not exactly one signing request: %q", body)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// Host names only: a public CA does not sign the node's private address.
	if !slices.Equal(csr.DNSNames, []string{"appliance.example.com"}) || len(csr.IPAddresses) != 0 {
		t.Errorf("request for %v %v, want the host names only", csr.DNSNames, csr.IPAddresses)
	}
	keyPEM := systemSecret(t, certs.CSRSecretName).Data[corev1.TLSPrivateKeyKey]
	if strings.Contains(string(body), "PRIVATE KEY") || strings.Contains(string(body), keyBody(keyPEM)) {
		t.Error("the answer carries the private key")
	}
	key, err := certs.ParsePrivateKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !csr.PublicKey.(*ecdsa.PublicKey).Equal(key.Public()) {
		t.Error("the pending key is not the request's")
	}
	if info := h.certificateInfo(s); !info.CsrPending || info.Source != CertificateInfoSourceBuiltin || info.Certificate != nil {
		t.Errorf("info %+v", info)
	}

	// A new request replaces the pending key.
	wantStatus(t, h.request(http.MethodPost, pathCSR, nil, s), http.StatusOK)
	if again := systemSecret(t, certs.CSRSecretName).Data[corev1.TLSPrivateKeyKey]; string(again) == string(keyPEM) {
		t.Error("the pending key was kept")
	}
}

func TestSignedCertificate(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)

	other, _ := ca.leaf(t)
	wantMessage(t, h.upload(http.MethodPut, pathSigned, map[string][]byte{"certificate": other, "chain": ca.pem}, s), http.StatusConflict,
		"There is no pending signing request; create one first.")

	rec := h.request(http.MethodPost, pathCSR, nil, s)
	wantStatus(t, rec, http.StatusOK)
	wantMessage(t, h.upload(http.MethodPut, pathSigned, map[string][]byte{"certificate": other, "chain": ca.pem}, s), http.StatusBadRequest,
		"The certificate was not issued for the pending signing request.")
	wantStatus(t, h.upload(http.MethodPut, pathSigned, map[string][]byte{"chain": ca.pem}, s), http.StatusBadRequest)
	pending := systemSecret(t, certs.CSRSecretName).Data[corev1.TLSPrivateKeyKey]

	signed := ca.sign(t, rec.Body.Bytes())
	wantStatus(t, h.upload(http.MethodPut, pathSigned, map[string][]byte{"certificate": signed, "chain": ca.pem}, s), http.StatusAccepted)
	if got := systemSecret(t, certs.CustomSecretName).Data[corev1.TLSPrivateKeyKey]; string(got) != string(pending) {
		t.Error("the installed key is not the pending one")
	}
	if !secretGone(t, certs.CSRSecretName) {
		t.Error("the pending key was kept after its certificate was installed")
	}
	if got := getAppliance(t).Spec.TLS; got.Source != v1alpha1.TLSSourceSecret || got.SecretRef != certs.CustomSecretName {
		t.Errorf("tls %+v", got)
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Action != string(AuditEntryActionCertificateSigned) || last.Result != string(Success) {
		t.Errorf("audit entry %+v", last)
	}
}

// writeACMECredentials stores the Secrets a custom ACME server leaves behind.
func writeACMECredentials(t *testing.T) {
	t.Helper()
	for _, name := range []string{certs.ACMEEABSecretName, certs.ACMEDNS01SecretName} {
		if err := testClient.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}, Data: map[string][]byte{"k": []byte("x")},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeavingACMEDeletesItsCredentials(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	certPEM, keyPEM := ca.leaf(t)

	writeACMECredentials(t)
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": keyPEM, "chain": ca.pem}, s), http.StatusAccepted)
	if !secretGone(t, certs.ACMEEABSecretName) || !secretGone(t, certs.ACMEDNS01SecretName) {
		t.Error("the ACME credentials stayed after an upload")
	}

	writeACMECredentials(t)
	wantStatus(t, h.request(http.MethodPut, pathBuiltin, nil, s), http.StatusAccepted)
	if !secretGone(t, certs.ACMEEABSecretName) || !secretGone(t, certs.ACMEDNS01SecretName) {
		t.Error("the ACME credentials stayed after back to built-in")
	}
	// Nothing to delete is no failure.
	wantStatus(t, h.request(http.MethodPut, pathBuiltin, nil, s), http.StatusAccepted)
}

func TestBackToBuiltin(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	certPEM, keyPEM := ca.leaf(t)
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": keyPEM, "chain": ca.pem}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPost, pathCSR, nil, s), http.StatusOK)

	wantStatus(t, h.request(http.MethodPut, pathBuiltin, nil, s), http.StatusAccepted)
	if !secretGone(t, certs.CustomSecretName) || !secretGone(t, certs.CSRSecretName) {
		t.Error("the uploaded certificate or the pending key was kept")
	}
	if got := getAppliance(t).Spec.TLS; got != (v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin}) {
		t.Errorf("tls %+v", got)
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Action != string(AuditEntryActionCertificateBuiltin) ||
		!slices.Equal(last.Paths, []string{"spec.tls.source", "spec.tls.secretRef"}) {
		t.Errorf("audit entry %+v", last)
	}

	// The built-in certificate is described once cert-manager issued it.
	builtin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: certs.SecretName},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
	if err := testClient.Create(context.Background(), builtin); err != nil {
		t.Fatal(err)
	}
	if info := h.certificateInfo(s); info.Source != CertificateInfoSourceBuiltin || info.CsrPending || info.Certificate == nil {
		t.Errorf("info %+v", info)
	}
	// Again, nothing changes.
	wantStatus(t, h.request(http.MethodPut, pathBuiltin, nil, s), http.StatusAccepted)
}

func TestCertificateChangesNeedSetup(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	ca := newTestCA(t)
	certPEM, keyPEM := ca.leaf(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"upload":  h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": keyPEM}, s),
		"csr":     h.request(http.MethodPost, pathCSR, nil, s),
		"builtin": h.request(http.MethodPut, pathBuiltin, nil, s),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s before setup: status %d, want 409", name, rec.Code)
		}
	}
}

func TestCertificateDetailsOfGarbage(t *testing.T) {
	if certificateDetails([]byte("garbage")) != nil {
		t.Error("details of no certificate")
	}
	if certificateDetails(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}})) != nil {
		t.Error("details of an invalid certificate")
	}
	if sentence("") != "" {
		t.Error("sentence of nothing")
	}
}

// failingDelete refuses to delete the object name.
type failingDelete struct {
	client.Client
	name string
}

func (c failingDelete) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetName() == c.name {
		return errors.New("delete refused")
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestSignedCertificateInstalledWhenTheRequestStays(t *testing.T) {
	h := newHarnessAs(t, failingDelete{Client: testClient, name: certs.CSRSecretName})
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	rec := h.request(http.MethodPost, pathCSR, nil, s)
	wantStatus(t, rec, http.StatusOK)

	wantStatus(t, h.upload(http.MethodPut, pathSigned, map[string][]byte{"certificate": ca.sign(t, rec.Body.Bytes()), "chain": ca.pem}, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.TLS; got.Source != v1alpha1.TLSSourceSecret {
		t.Errorf("tls %+v, want the signed certificate in use", got)
	}
	if !strings.Contains(h.logs.String(), "signing request not deleted") {
		t.Errorf("log %s, want the failed deletion", h.logs)
	}
}

type obj = map[string]any

const (
	acmeDirectory = "https://acme.example.com/directory"
	acmeUUID      = "7f1c2a3e-1111-4222-8333-444455556666"
)

var acmeILM = obj{"raProfileUuid": acmeUUID, "vaultUuid": acmeUUID, "vaultProfileUuid": acmeUUID}

func cloudflare(config, credentials obj) obj {
	return obj{"type": "dns01", "dns01": obj{"provider": "cloudflare", "config": config, "credentials": credentials}}
}

func TestACMERefusals(t *testing.T) {
	tests := []struct {
		name    string
		body    obj
		message string
	}{
		{"plain http", obj{"directoryUrl": "http://acme.example.com/directory"}, "The ACME directory must be an https URL."},
		{"not a URL", obj{"directoryUrl": "acme"}, "The ACME directory must be an https URL."},
		{"neither", obj{"email": "a@example.com"}, "Give a directory URL or the ILM profile."},
		{"both", obj{"directoryUrl": acmeDirectory, "ilm": acmeILM}, "Give either a directory URL or the ILM profile, not both."},
		{"ILM without a vault", obj{"ilm": obj{"raProfileUuid": acmeUUID}}, "The ILM profile needs the RA profile, the vault and the vault profile."},
		{"EAB id without HMAC", obj{"directoryUrl": acmeDirectory, "eabKeyId": "kid"}, "The external account binding needs both its key ID and its HMAC key."},
		{"EAB HMAC without id", obj{"directoryUrl": acmeDirectory, "eabHmac": "aGVsbG8"}, "The external account binding needs both its key ID and its HMAC key."},
		{"CA bundle", obj{"directoryUrl": acmeDirectory, "caBundle": "not pem"}, "The CA bundle holds no certificate."},
		{"solver type", obj{"directoryUrl": acmeDirectory, "solver": obj{"type": "tls-alpn-01"}}, "The solver must be http01 or dns01."},
		{"dns01 without provider", obj{"directoryUrl": acmeDirectory, "solver": obj{"type": "dns01"}}, "Choose a DNS provider."},
		{"unknown provider", obj{"directoryUrl": acmeDirectory, "solver": obj{"type": "dns01", "dns01": obj{"provider": "godaddy"}}},
			`The DNS provider "godaddy" is not supported.`},
		{"unknown config key", obj{"directoryUrl": acmeDirectory, "solver": cloudflare(obj{"zone": "x"}, obj{"apiToken": "t"})},
			`The DNS provider has no setting "zone".`},
		{"unknown credential key", obj{"directoryUrl": acmeDirectory, "solver": cloudflare(nil, obj{"apiToken": "t", "apiKey": "k"})},
			`The DNS provider has no credential "apiKey".`},
		{"missing credential", obj{"directoryUrl": acmeDirectory, "solver": cloudflare(nil, nil)},
			`The DNS provider needs the credential "apiToken".`},
		{"missing setting", obj{"directoryUrl": acmeDirectory, "solver": obj{"type": "dns01", "dns01": obj{"provider": "acmedns", "credentials": obj{"accountJSON": "{}"}}}},
			`The DNS provider needs the setting "host".`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			s := h.signIn()
			createCertificateAppliance(t)
			wantMessage(t, h.request(http.MethodPut, pathACME, tt.body, s), http.StatusBadRequest, tt.message)
			if got := getAppliance(t).Spec.TLS; got.Source != v1alpha1.TLSSourceBuiltin || got.ACME != nil {
				t.Errorf("tls %+v after a refusal", got)
			}
			if !secretGone(t, certs.ACMEDNS01SecretName) || !secretGone(t, certs.ACMEEABSecretName) {
				t.Error("a refused request stored a Secret")
			}
		})
	}
}

func TestACMENotSetUp(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodPut, pathACME, obj{"directoryUrl": acmeDirectory}, s), http.StatusConflict)
}

func TestACMECustomDirectory(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	ca := newTestCA(t)
	// A binding from an earlier request leaves nothing but the new HMAC key behind.
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: certs.ACMEEABSecretName},
		Data:       map[string][]byte{"keyId": []byte("old"), "hmac": []byte("old")},
	}); err != nil {
		t.Fatal(err)
	}

	body := obj{"directoryUrl": acmeDirectory, "email": "ops@example.com", "eabKeyId": "kid", "eabHmac": "aG1hYw",
		"caBundle": string(ca.pem), "solver": obj{"type": "http01"}}
	wantStatus(t, h.request(http.MethodPut, pathACME, body, s), http.StatusAccepted)

	got := getAppliance(t).Spec.TLS
	want := &v1alpha1.ACMESpec{
		DirectoryURL: acmeDirectory, Email: "ops@example.com", EABKeyID: "kid",
		EABSecretRef: &corev1.LocalObjectReference{Name: certs.ACMEEABSecretName}, CABundle: ca.pem,
		Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01},
	}
	if got.Source != v1alpha1.TLSSourceACME || got.ACME == nil || !equalACME(got.ACME, want) {
		t.Errorf("tls %+v, want acme %+v", got, want)
	}
	if data := systemSecret(t, certs.ACMEEABSecretName).Data; len(data) != 1 || string(data["hmac"]) != "aG1hYw" {
		t.Errorf("EAB Secret keys %v, want only hmac", secretKeys(data))
	}
	if !secretGone(t, certs.ACMEDNS01SecretName) {
		t.Error("an http01 solver stored DNS credentials")
	}
	entries := h.entries()
	last := entries[len(entries)-1]
	if last.Action != string(AuditEntryActionCertificateAcme) || last.Result != string(Success) ||
		last.Details["solver"] != "http01" || last.Details["provider"] != "" ||
		!slices.Equal(last.Paths, []string{"spec.tls.source", "spec.tls.acme"}) {
		t.Errorf("audit entry %+v", last)
	}
	for _, v := range last.Details {
		if strings.Contains(v, "aG1hYw") {
			t.Error("the audit entry holds the HMAC key")
		}
	}

	info := h.certificateInfo(s)
	if info.Source != CertificateInfoSourceAcme || info.Acme == nil || *info.Acme.DirectoryUrl != acmeDirectory ||
		*info.Acme.Email != "ops@example.com" || *info.Acme.EabKeyId != "kid" || !*info.Acme.EabHmacSet ||
		info.Acme.CaBundle == nil || *info.Acme.CaBundle != string(ca.pem) || info.Acme.Solver.Type != AcmeSolverInfoTypeHttp01 || info.Acme.LastError != nil {
		t.Errorf("info %+v", info)
	}
	if rec := h.request(http.MethodGet, pathCertificate, nil, s); strings.Contains(rec.Body.String(), "aG1hYw") {
		t.Error("GET shows the HMAC key")
	}

	// Without a binding the Secret goes, and the solver defaults to http01.
	wantStatus(t, h.request(http.MethodPut, pathACME, obj{"directoryUrl": acmeDirectory}, s), http.StatusAccepted)
	if !secretGone(t, certs.ACMEEABSecretName) {
		t.Error("the EAB Secret stayed without a binding")
	}
	got = getAppliance(t).Spec.TLS
	if got.ACME.EABKeyID != "" || got.ACME.EABSecretRef != nil || got.ACME.Solver.Type != v1alpha1.ACMESolverHTTP01 {
		t.Errorf("acme %+v", got.ACME)
	}
}

func TestACMEDNS01(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)

	body := obj{"directoryUrl": acmeDirectory, "solver": obj{"type": "dns01", "dns01": obj{
		"provider": "rfc2136", "config": obj{"nameserver": "192.0.2.53:53", "tsigKeyName": "acme"}, "credentials": obj{"tsigSecret": "s3cret"}}}}
	wantStatus(t, h.request(http.MethodPut, pathACME, body, s), http.StatusAccepted)

	solver := getAppliance(t).Spec.TLS.ACME.Solver
	if solver.Type != v1alpha1.ACMESolverDNS01 || solver.DNS01 == nil || solver.DNS01.Provider != "rfc2136" ||
		solver.DNS01.Config["nameserver"] != "192.0.2.53:53" || solver.DNS01.Config["tsigKeyName"] != "acme" ||
		solver.DNS01.CredentialsSecretRef == nil || solver.DNS01.CredentialsSecretRef.Name != certs.ACMEDNS01SecretName {
		t.Errorf("solver %+v", solver)
	}
	if data := systemSecret(t, certs.ACMEDNS01SecretName).Data; len(data) != 1 || string(data["tsigSecret"]) != "s3cret" {
		t.Errorf("DNS Secret keys %v", secretKeys(data))
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Details["provider"] != "rfc2136" || last.Details["solver"] != "dns01" {
		t.Errorf("audit details %v", last.Details)
	}
	rec := h.request(http.MethodGet, pathCertificate, nil, s)
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Error("GET shows a credential")
	}
	info := h.certificateInfo(s)
	d := info.Acme.Solver.Dns01
	if d == nil || d.Provider != "rfc2136" || !slices.Equal(d.CredentialKeys, []string{"tsigSecret"}) || d.Config["nameserver"] != "192.0.2.53:53" {
		t.Errorf("dns01 info %+v", d)
	}

	// Credentials of a provider that needs none of the stored keys replace them whole.
	body = obj{"directoryUrl": acmeDirectory, "solver": cloudflare(nil, obj{"apiToken": "tok"})}
	wantStatus(t, h.request(http.MethodPut, pathACME, body, s), http.StatusAccepted)
	if data := systemSecret(t, certs.ACMEDNS01SecretName).Data; len(data) != 1 || string(data["apiToken"]) != "tok" {
		t.Errorf("DNS Secret keys %v", secretKeys(data))
	}

	wantStatus(t, h.request(http.MethodPut, pathACME, obj{"directoryUrl": acmeDirectory}, s), http.StatusAccepted)
	if !secretGone(t, certs.ACMEDNS01SecretName) {
		t.Error("the DNS credentials stayed after switching to http01")
	}
	if getAppliance(t).Spec.TLS.ACME.Solver.DNS01 != nil {
		t.Error("the spec kept the DNS solver")
	}
}

func TestACMEILM(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	for _, name := range []string{certs.ACMEEABSecretName, certs.ACMEDNS01SecretName} {
		if err := testClient.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}, Data: map[string][]byte{"hmac": []byte("x")},
		}); err != nil {
			t.Fatal(err)
		}
	}

	wantStatus(t, h.request(http.MethodPut, pathACME, obj{"ilm": acmeILM, "solver": cloudflare(nil, obj{"apiToken": "t"})}, s), http.StatusAccepted)

	got := getAppliance(t).Spec.TLS
	want := &v1alpha1.ACMESpec{
		ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: acmeUUID, VaultUUID: acmeUUID, VaultProfileUUID: acmeUUID, ProfileName: "appliance"},
		Solver:     v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverHTTP01},
	}
	if got.Source != v1alpha1.TLSSourceACME || !equalACME(got.ACME, want) {
		t.Errorf("tls %+v, want acme %+v", got, want)
	}
	if !secretGone(t, certs.ACMEDNS01SecretName) || !secretGone(t, certs.ACMEEABSecretName) {
		t.Error("the custom directory's Secrets stayed with the ILM profile")
	}
	info := h.certificateInfo(s)
	if info.Acme == nil || info.Acme.Ilm == nil || info.Acme.Ilm.VaultUuid != acmeUUID || info.Acme.DirectoryUrl != nil {
		t.Errorf("info %+v", info.Acme)
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Details["solver"] != "ilm" {
		t.Errorf("audit details %v", last.Details)
	}
}

func TestACMELastErrorAndBuiltin(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	wantStatus(t, h.request(http.MethodPut, pathACME, obj{"directoryUrl": acmeDirectory}, s), http.StatusAccepted)
	if info := h.certificateInfo(s); info.Acme.LastError != nil {
		t.Errorf("last error %q without a condition", *info.Acme.LastError)
	}

	a := getAppliance(t)
	meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionCertificateReady,
		Status: metav1.ConditionFalse, Reason: "Failed", Message: "the ACME server refused the order"})
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if info := h.certificateInfo(s); info.Acme.LastError == nil || *info.Acme.LastError != "the ACME server refused the order" {
		t.Errorf("info %+v", info.Acme)
	}

	wantStatus(t, h.request(http.MethodPut, pathBuiltin, nil, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.TLS; got.Source != v1alpha1.TLSSourceBuiltin || got.ACME != nil {
		t.Errorf("tls %+v", got)
	}
	if info := h.certificateInfo(s); info.Source != CertificateInfoSourceBuiltin || info.Acme != nil {
		t.Errorf("info %+v", info)
	}
}

func equalACME(a, b *v1alpha1.ACMESpec) bool { return reflect.DeepEqual(a, b) }

func secretKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	return keys
}

const pathILMOptions = "/appliance/api/v1/certificate/acme/ilm-options"

// fakeILMProfiles returns an ILM function of a fake ILM that lists RA profiles and vault profiles, or answers 500
// when down is set, and counts its use.
func fakeILMProfiles(t *testing.T, down *bool) (func(context.Context, *v1alpha1.Appliance) (*ilmclient.Client, error), *int) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case *down:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/raProfiles":
			_, _ = w.Write([]byte(`[{"uuid":"r1","name":"web","enabled":true},{"uuid":"r2","name":"off","enabled":false}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/vaultProfiles/list":
			_, _ = w.Write([]byte(`{"items":[{"uuid":"vp1","name":"hashicorp","enabled":true,"vaultInstance":{"uuid":"v1"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	calls := 0
	return func(context.Context, *v1alpha1.Appliance) (*ilmclient.Client, error) {
		calls++
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		return ilmclient.New(srv.Listener.Addr().String(), "example.com", tls.Certificate{}, pool), nil
	}, &calls
}

// setILMReady sets the conditions that tell that ILM is ready.
func setILMReady(t *testing.T) {
	t.Helper()
	a := getAppliance(t)
	for _, c := range []string{v1alpha1.ConditionPlatformReady, v1alpha1.ConditionILMSettingsApplied} {
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: c, Status: metav1.ConditionTrue, Reason: "Ready", Message: "ready"})
	}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func TestACMEILMOptions(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createCertificateAppliance(t)
	down := false
	ilm, calls := fakeILMProfiles(t, &down)
	h.srv.deps.ILM = ilm
	audited := len(h.entries())

	rec := h.request(http.MethodGet, pathILMOptions, nil, s)
	wantMessage(t, rec, http.StatusConflict, "ILM is not ready yet; try again shortly.")
	if *calls != 0 {
		t.Errorf("reached ILM %d times before it is ready", *calls)
	}

	setILMReady(t)
	rec = h.request(http.MethodGet, pathILMOptions, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got AcmeIlmOptions
	decode(t, rec, &got)
	want := AcmeIlmOptions{
		RaProfiles:    []AcmeIlmRaProfile{{Uuid: "r1", Name: "web"}},
		VaultProfiles: []AcmeIlmVaultProfile{{Uuid: "vp1", Name: "hashicorp", VaultUuid: "v1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options %+v, want %+v", got, want)
	}

	down = true
	wantMessage(t, h.request(http.MethodGet, pathILMOptions, nil, s), http.StatusConflict, "ILM is not ready yet; try again shortly.")
	if n := len(h.entries()); n != audited {
		t.Errorf("%d audit entries for reads, want none", n-audited)
	}
}

func TestACMEILMOptionsWithoutILMOrSetup(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodGet, pathILMOptions, nil, s), http.StatusConflict)
	createCertificateAppliance(t)
	setILMReady(t)
	wantMessage(t, h.request(http.MethodGet, pathILMOptions, nil, s), http.StatusConflict, "ILM is not ready yet; try again shortly.")
}
