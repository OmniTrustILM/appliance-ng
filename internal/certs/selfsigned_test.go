package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func TestSelfSigned(t *testing.T) {
	from := time.Now().Truncate(time.Second)
	cert, err := SelfSigned("ILM", []string{"127.0.0.1", "localhost", "10.0.2.15"}, from, from.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || key.Curve != elliptic.P256() {
		t.Errorf("public key %T, want ECDSA P-256", leaf.PublicKey)
	}
	if leaf.Subject.CommonName != "ILM" || !leaf.NotBefore.Equal(from) || !leaf.NotAfter.Equal(from.Add(time.Hour)) {
		t.Errorf("subject %q valid %s to %s", leaf.Subject.CommonName, leaf.NotBefore, leaf.NotAfter)
	}
	if len(leaf.IPAddresses) != 2 || !leaf.IPAddresses[1].Equal(net.ParseIP("10.0.2.15")) {
		t.Errorf("IP SANs %v", leaf.IPAddresses)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Errorf("DNS SANs %v", leaf.DNSNames)
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		t.Errorf("not self-signed: %v", err)
	}
}
