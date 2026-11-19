package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	certNodeAddress = "192.0.2.10"
	pathCertificate = "/api/v1/certificate"
	pathCSR         = "/api/v1/certificate/csr"
	pathSigned      = "/api/v1/certificate/csr/signed"
	pathBuiltin     = "/api/v1/certificate/builtin"
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
