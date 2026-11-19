package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNewCSR(t *testing.T) {
	csrPEM, secret, err := NewCSR(uploadHosts)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("not a PEM signing request: %q", csrPEM)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("signature: %v", err)
	}
	if csr.Subject.CommonName != uploadHosts[0] || !reflect.DeepEqual(csr.DNSNames, uploadHosts) {
		t.Errorf("names %q %v", csr.Subject.CommonName, csr.DNSNames)
	}
	if len(csr.IPAddresses) != 0 {
		t.Errorf("addresses %v, want none: public CAs do not sign private addresses", csr.IPAddresses)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("key %T, want ECDSA P-256", csr.PublicKey)
	}

	if secret.Namespace != Namespace || secret.Name != CSRSecretName || secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret %s/%s of type %s", secret.Namespace, secret.Name, secret.Type)
	}
	key, err := ParsePrivateKey(secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(key.Public()) {
		t.Error("the Secret's key is not the request's")
	}
	if body := pemBody(secret.Data[corev1.TLSPrivateKeyKey]); body == "" || strings.Contains(string(csrPEM), body) {
		t.Error("the signing request carries the private key")
	}
}

func TestNewCSRNeedsAHost(t *testing.T) {
	if _, _, err := NewCSR(nil); err == nil {
		t.Error("a signing request without a host name was created")
	}
}

func TestNewCSRLeavesALongNameOutOfTheCommonName(t *testing.T) {
	long := strings.Repeat("a", 60) + ".example.com"
	csrPEM, _, err := NewCSR([]string{long})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if csr.Subject.CommonName != "" || !reflect.DeepEqual(csr.DNSNames, []string{long}) {
		t.Errorf("names %q %v, want no common name and the host as a DNS name", csr.Subject.CommonName, csr.DNSNames)
	}
}
