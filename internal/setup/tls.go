package setup

import (
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

// certValidity is how long the setup certificate is valid.
const certValidity = 30 * 24 * time.Hour

// SelfSigned returns a self-signed ECDSA P-256 server certificate for hosts, as IP addresses or DNS names, and the
// SHA-256 fingerprint of its DER encoding as colon-separated upper-case hex bytes. The certificate is valid from one
// hour ago for 30 days, so that a browser whose clock is a little behind still accepts it. The key stays in memory.
func SelfSigned(hosts []string) (tls.Certificate, string, error) {
	if len(hosts) == 0 {
		return tls.Certificate{}, "", errors.New("setup certificate: no host")
	}
	now := time.Now().Truncate(time.Second)
	cert, err := certs.SelfSigned(hosts[0], hosts, now.Add(-time.Hour), now.Add(certValidity))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("setup certificate: %w", err)
	}
	return cert, fingerprint(cert.Certificate[0]), nil
}

// fingerprint returns the SHA-256 of der as colon-separated upper-case hex bytes, as browsers show it.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
