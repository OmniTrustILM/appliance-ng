package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

const credentialPassword = "a long enough password"

// adminCredential is the administrator certificate and key, in PEM, as cert-manager writes them.
type adminCredential struct {
	cert, key []byte
	leaf      *x509.Certificate
}

func newAdminCredential(t *testing.T, keyPEM func(*rsa.PrivateKey) *pem.Block) adminCredential {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ilm internal CA"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "admin"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return adminCredential{
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:  pem.EncodeToMemory(keyPEM(key)),
		leaf: leaf,
	}
}

func pkcs1(key *rsa.PrivateKey) *pem.Block {
	return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
}

func pkcs8(key *rsa.PrivateKey) *pem.Block {
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	return &pem.Block{Type: "PRIVATE KEY", Bytes: der}
}

func createAdminSecret(t *testing.T, cred adminCredential) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: AdminCredentialSecret},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: cred.cert, corev1.TLSPrivateKeyKey: cred.key},
	}
	if err := testClient.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
}

func TestAdminCredentialOnce(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	cred := newAdminCredential(t, pkcs1)
	createAdminSecret(t, cred)

	rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "application/x-pkcs12" {
		t.Errorf("Content-Type %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got == "" {
		t.Error("no Content-Disposition")
	}
	key, leaf, err := pkcs12.Decode(rec.Body.Bytes(), credentialPassword)
	if err != nil {
		t.Fatalf("decode the PKCS#12 file: %v", err)
	}
	if !leaf.Equal(cred.leaf) {
		t.Error("the PKCS#12 file holds another certificate")
	}
	if rsaKey, ok := key.(*rsa.PrivateKey); !ok || !rsaKey.PublicKey.Equal(cred.leaf.PublicKey) {
		t.Error("the PKCS#12 file holds another key")
	}
	secret := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		t.Fatal(err)
	}
	if _, ok := secret.Annotations[DownloadedAnnotation]; !ok {
		t.Errorf("the Secret lacks annotation %s: %v", DownloadedAnnotation, secret.Annotations)
	}

	rec = h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s)
	wantStatus(t, rec, http.StatusGone)

	var credentialEntries []string
	for _, e := range h.entries() {
		if e.Action == string(AuditEntryActionAdminCredential) {
			credentialEntries = append(credentialEntries, e.Result)
		}
	}
	if len(credentialEntries) != 2 || credentialEntries[0] != string(Success) || credentialEntries[1] != string(Failure) {
		t.Errorf("admin credential audit results = %v, want success then failure", credentialEntries)
	}
}

// TestAdminCredentialAfterRenewal checks that a renewed certificate can be downloaded, once, although the one it
// replaced was.
func TestAdminCredentialAfterRenewal(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAdminSecret(t, newAdminCredential(t, pkcs1))
	body := map[string]string{"password": credentialPassword}
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", body, s), http.StatusOK)

	renewed := newAdminCredential(t, pkcs1)
	ctx := context.Background()
	secret := &corev1.Secret{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data = map[string][]byte{corev1.TLSCertKey: renewed.cert, corev1.TLSPrivateKeyKey: renewed.key}
	if err := testClient.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}

	rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", body, s)
	wantStatus(t, rec, http.StatusOK)
	if _, leaf, err := pkcs12.Decode(rec.Body.Bytes(), credentialPassword); err != nil || !leaf.Equal(renewed.leaf) {
		t.Errorf("the download after renewal does not hold the renewed certificate (error %v)", err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(renewed.cert)
	if got, want := secret.Annotations[DownloadedAnnotation], hex.EncodeToString(sum[:]); got != want {
		t.Errorf("annotation %s = %q, want the renewed certificate's SHA-256 %q", DownloadedAnnotation, got, want)
	}
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", body, s), http.StatusGone)
}

func TestAdminCredentialAcceptsAPKCS8Key(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAdminSecret(t, newAdminCredential(t, pkcs8))
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s), http.StatusOK)
}

func TestAdminCredentialRefuses(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s), http.StatusNotFound)

	createAdminSecret(t, newAdminCredential(t, pkcs1))
	for _, body := range []any{
		map[string]string{},
		map[string]string{"password": "short"},
		map[string]string{"password": "unbelievable"},      // one of the most common passwords
		map[string]string{"password": "my admin password"}, // holds the user name
	} {
		rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", body, s)
		wantStatus(t, rec, http.StatusBadRequest)
		var got Error
		decode(t, rec, &got)
		if got.Field == nil || *got.Field != "password" {
			t.Errorf("field = %v, want password", got.Field)
		}
	}
}

func TestAdminCredentialWithABrokenSecret(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAdminSecret(t, adminCredential{cert: []byte("not a certificate"), key: []byte("not a key")})
	rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	secret := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		t.Fatal(err)
	}
	if _, ok := secret.Annotations[DownloadedAnnotation]; ok {
		t.Error("a failed download marks the credential as downloaded")
	}
}

// TestAdminCredentialFailsClosedWithoutAudit checks that the credential is not handed out, nor used up, when its
// audit entry cannot be written.
func TestAdminCredentialFailsClosedWithoutAudit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAdminSecret(t, newAdminCredential(t, pkcs1))
	h.srv.deps.Audit = failingAudit{}
	rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if rec.Header().Get("Content-Type") == "application/x-pkcs12" {
		t.Error("the PKCS#12 file was sent without an audit entry")
	}
	secret := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		t.Fatal(err)
	}
	if _, ok := secret.Annotations[DownloadedAnnotation]; ok {
		t.Error("the credential is used up although nothing was handed out")
	}

	h.srv.deps.Audit = h.audit
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s), http.StatusOK)
}

var (
	oidTripleDESPBE = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidPBES2        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidSHA1         = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA256       = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
)

func hasOID(der []byte, oid asn1.ObjectIdentifier) bool {
	enc, _ := asn1.Marshal(oid)
	return bytes.Contains(der, enc)
}

// TestAdminCredentialFormat checks the encryption of the file: the compatible default imports into macOS Keychain,
// which cannot read AES-256, and the modern format uses PBES2 with AES-256 and a SHA-256 MAC.
func TestAdminCredentialFormat(t *testing.T) {
	tests := []struct {
		name   string
		body   map[string]string
		format string
		modern bool
	}{
		{"default", map[string]string{"password": credentialPassword}, "compatible", false},
		{"compatible", map[string]string{"password": credentialPassword, "format": "compatible"}, "compatible", false},
		{"modern", map[string]string{"password": credentialPassword, "format": "modern"}, "modern", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			s := h.signIn()
			cred := newAdminCredential(t, pkcs1)
			createAdminSecret(t, cred)

			rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", tt.body, s)
			wantStatus(t, rec, http.StatusOK)
			pfx := rec.Body.Bytes()
			if _, leaf, _, err := pkcs12.DecodeChain(pfx, credentialPassword); err != nil || !leaf.Equal(cred.leaf) {
				t.Fatalf("decode the PKCS#12 file: %v", err)
			}
			if got := hasOID(pfx, oidPBES2); got != tt.modern {
				t.Errorf("PBES2 in the file = %v, want %v", got, tt.modern)
			}
			if got := hasOID(pfx, oidTripleDESPBE); got == tt.modern {
				t.Errorf("3DES in the file = %v, want %v", got, !tt.modern)
			}
			wantMAC, otherMAC := oidSHA1, oidSHA256
			if tt.modern {
				wantMAC, otherMAC = oidSHA256, oidSHA1
			}
			if !hasOID(pfx, wantMAC) || hasOID(pfx, otherMAC) {
				t.Errorf("the MAC of the file is not %v", wantMAC)
			}
			var details map[string]string
			for _, e := range h.entries() {
				if e.Action == string(AuditEntryActionAdminCredential) {
					details = e.Details
				}
			}
			if details["format"] != tt.format {
				t.Errorf("audit details = %v, want format %s", details, tt.format)
			}
		})
	}
}

func TestAdminCredentialUnknownFormat(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAdminSecret(t, newAdminCredential(t, pkcs1))

	rec := h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword, "format": "x"}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	var answer Error
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Field == nil || *answer.Field != "format" {
		t.Errorf("answer %s, want field format", rec.Body)
	}
	// Nothing was handed out: the same request with a valid format still works.
	rec = h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s)
	wantStatus(t, rec, http.StatusOK)
}
