package setup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSelfSigned(t *testing.T) {
	cert, fingerprint, err := SelfSigned([]string{"10.0.2.15", "ilm.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		t.Errorf("public key %T, want ECDSA P-256", leaf.PublicKey)
	}
	// Valid from an hour back, for a browser whose clock is a little behind the appliance's.
	if got := leaf.NotAfter.Sub(leaf.NotBefore); got != 30*24*time.Hour+time.Hour {
		t.Errorf("validity %s, want 30 days and an hour", got)
	}
	if time.Now().Add(-59 * time.Minute).Before(leaf.NotBefore) {
		t.Errorf("not before %s, want an hour ago", leaf.NotBefore)
	}
	if time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		t.Errorf("not valid now: %s to %s", leaf.NotBefore, leaf.NotAfter)
	}
	if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("10.0.2.15")) {
		t.Errorf("IP SANs %v, want 10.0.2.15", leaf.IPAddresses)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "ilm.example.com" {
		t.Errorf("DNS SANs %v", leaf.DNSNames)
	}
	if err := leaf.VerifyHostname("10.0.2.15"); err != nil {
		t.Error(err)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("extended key usage %v, want server authentication", leaf.ExtKeyUsage)
	}

	sum := sha256.Sum256(cert.Certificate[0])
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	if want := strings.Join(parts, ":"); fingerprint != want {
		t.Errorf("fingerprint %q, want %q", fingerprint, want)
	}

	_, other, err := SelfSigned([]string{"10.0.2.15"})
	if err != nil || other == fingerprint {
		t.Errorf("second certificate: fingerprint %q, %v; want a new key", other, err)
	}
}

func TestSelfSignedNeedsAHost(t *testing.T) {
	if _, _, err := SelfSigned(nil); err == nil {
		t.Error("SelfSigned accepted no host")
	}
}
