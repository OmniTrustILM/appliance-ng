package certs

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The Secret, in Namespace, that holds the bundle of additional trusted CA certificates spec.trust references.
const (
	TrustSecretName = "appliance-trust"
	TrustKey        = "ca.crt"
)

// CertSummary describes a certificate of a trust bundle.
type CertSummary struct {
	Subject, Issuer string
	NotAfter        time.Time
	// SHA256 is the lowercase hex SHA-256 of the certificate's DER, which identifies it in the bundle.
	SHA256 string
	// New says that a merge adds the certificate; the current bundle does not hold it.
	New bool
}

// RejectedError is why certificates offered for the trust bundle were refused. Its text names the problem and
// never holds key material.
type RejectedError struct{ reason string }

func (e *RejectedError) Error() string { return e.reason }

func rejected(format string, args ...any) error {
	return &RejectedError{reason: fmt.Sprintf(format, args...)}
}

// MergeBundle adds the CA certificates of added, in PEM, to the bundle current and returns the merged bundle and a
// summary of each certificate in it, in bundle order, those that added contributes marked new. A certificate
// already in the bundle, by the SHA-256 of its DER, is not added again. Added must hold certificates only, all of
// them CA certificates; otherwise nothing is merged and the error, a *RejectedError, names the problem and the
// subjects of the certificates that are not CAs.
func MergeBundle(current, added []byte) ([]byte, []CertSummary, error) {
	bundle, err := parseCertificates(current)
	if err != nil {
		return nil, nil, fmt.Errorf("read the current trust bundle: %w", err)
	}
	offered, err := parseCertificates(added)
	if err != nil {
		return nil, nil, rejected("the upload cannot be read as PEM certificates: %v", err)
	}
	if len(offered) == 0 {
		return nil, nil, rejected("the upload holds no certificate")
	}
	var notCA []string
	for _, c := range offered {
		if !c.BasicConstraintsValid || !c.IsCA {
			notCA = append(notCA, c.Subject.String())
		}
	}
	if len(notCA) > 0 {
		return nil, nil, rejected("only CA certificates can be trusted, and these are not: %s", strings.Join(notCA, "; "))
	}

	preview := summaries(bundle)
	for _, c := range offered {
		s := summary(c)
		if slices.ContainsFunc(preview, func(p CertSummary) bool { return p.SHA256 == s.SHA256 }) {
			continue
		}
		s.New = true
		bundle = append(bundle, c)
		preview = append(preview, s)
	}
	return encodeCertificates(bundle), preview, nil
}

// ParseBundle summarises the certificates of a trust bundle, in bundle order.
func ParseBundle(bundle []byte) ([]CertSummary, error) {
	certs, err := parseCertificates(bundle)
	if err != nil {
		return nil, fmt.Errorf("read the trust bundle: %w", err)
	}
	return summaries(certs), nil
}

// RemoveFromBundle returns bundle without the certificate whose DER has the SHA-256 fingerprint, in hex, and
// whether the bundle held it. The result is empty when that was the last certificate.
func RemoveFromBundle(bundle []byte, fingerprint string) ([]byte, bool, error) {
	certs, err := parseCertificates(bundle)
	if err != nil {
		return nil, false, fmt.Errorf("read the trust bundle: %w", err)
	}
	fingerprint = strings.ToLower(fingerprint)
	before := len(certs)
	left := slices.DeleteFunc(certs, func(c *x509.Certificate) bool { return sha256Hex(c) == fingerprint })
	if len(left) == before {
		return bundle, false, nil
	}
	return encodeCertificates(left), true, nil
}

func encodeCertificates(certs []*x509.Certificate) []byte {
	var out bytes.Buffer
	for _, c := range certs {
		// Encoding to a buffer cannot fail.
		_ = pem.Encode(&out, &pem.Block{Type: blockCertificate, Bytes: c.Raw})
	}
	return out.Bytes()
}

func summaries(certs []*x509.Certificate) []CertSummary {
	out := make([]CertSummary, len(certs))
	for i, c := range certs {
		out[i] = summary(c)
	}
	return out
}

func summary(c *x509.Certificate) CertSummary {
	return CertSummary{Subject: c.Subject.String(), Issuer: c.Issuer.String(), NotAfter: c.NotAfter.UTC(), SHA256: sha256Hex(c)}
}

func sha256Hex(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}
