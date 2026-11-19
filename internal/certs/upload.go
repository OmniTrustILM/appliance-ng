package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CustomSecretName is the TLS Secret, in Namespace, that holds an uploaded certificate, its chain and its key.
const CustomSecretName = "appliance-tls-custom"

// minRSABits is the smallest RSA key an uploaded certificate may have.
const minRSABits = 2048

const (
	blockCertificate = "CERTIFICATE"
	dateFormat       = "2006-01-02 15:04 MST"
)

// Errors of ValidateUpload that callers tell apart. The messages of all its errors name the problem and never
// contain key material.
var (
	ErrKeyMismatch = errors.New("the private key does not match the certificate")
	errNoCert      = errors.New("the certificate is not a PEM-encoded X.509 certificate")
	errNoKey       = errors.New("the private key is not an unencrypted PEM-encoded RSA or ECDSA key")
	errKeyType     = errors.New("the certificate's key is neither RSA nor ECDSA with P-256, P-384 or P-521")
	errChain       = errors.New("the chain does not lead to a trusted root certificate; add the intermediate and root certificates")
	errUsage       = errors.New("the certificate or its chain is not for server authentication")
	errSelfSigned  = errors.New("the certificate is self-signed; upload one issued by a certificate authority or keep the built-in certificate")
)

// ValidateUpload checks a certificate before the appliance serves it: certPEM holds the certificate, optionally
// followed by its chain, keyPEM its private key and chainPEM further chain certificates. The certificate must
// match the key, use RSA with at least 2048 bits or ECDSA, be valid at now, cover hosts, not be self-signed, and
// chain to a root certificate of the upload or of roots. It need not cover the node address, which certificates
// of public CAs cannot. An error names the first problem found.
func ValidateUpload(certPEM, keyPEM, chainPEM []byte, hosts []string, roots *x509.CertPool, now time.Time) error {
	uploaded, err := parseCertificates(certPEM)
	if err != nil || len(uploaded) == 0 {
		return errNoCert
	}
	leaf := uploaded[0]
	chain, err := parseCertificates(chainPEM)
	if err != nil {
		return errors.New("the chain is not a list of PEM-encoded X.509 certificates")
	}
	key, err := ParsePrivateKey(keyPEM)
	if err != nil {
		return err
	}
	if err := checkKeyStrength(leaf.PublicKey); err != nil {
		return err
	}
	if !samePublicKey(leaf.PublicKey, key.Public()) {
		return ErrKeyMismatch
	}
	if err := checkValidity(leaf, now); err != nil {
		return err
	}
	if err := checkNames(leaf, hosts); err != nil {
		return err
	}
	if bytes.Equal(leaf.RawIssuer, leaf.RawSubject) && leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil {
		return errSelfSigned
	}
	return verifyChain(leaf, append(uploaded[1:], chain...), roots, now)
}

// TrustedRoots are the system's root certificates and the certificates of bundle, the appliance's additional
// trusted CAs. Without the system's roots it returns those of bundle and the error.
func TrustedRoots(bundle []byte) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	roots.AppendCertsFromPEM(bundle)
	return roots, err
}

// ParsePrivateKey parses the first PRIVATE KEY block of keyPEM, skipping others such as EC PARAMETERS, as a
// PKCS #8, PKCS #1 or SEC 1 private key. Its error never contains key material.
func ParsePrivateKey(keyPEM []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(keyPEM)
	for block != nil && !strings.HasSuffix(block.Type, "PRIVATE KEY") {
		block, rest = pem.Decode(rest)
	}
	if block == nil {
		return nil, errNoKey
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, errNoKey
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errNoKey
}

// parseCertificates parses the CERTIFICATE blocks of data and fails on any other content.
func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != blockCertificate {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(bytes.TrimSpace(data)) != 0 {
		return nil, errors.New("data after the last PEM block")
	}
	return out, nil
}

func checkKeyStrength(pub crypto.PublicKey) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if bits := k.N.BitLen(); bits < minRSABits {
			return fmt.Errorf("the certificate's RSA key has %d bits; at least %d are needed", bits, minRSABits)
		}
		return nil
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		}
	}
	return errKeyType
}

func samePublicKey(a, b crypto.PublicKey) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}

func checkValidity(leaf *x509.Certificate, now time.Time) error {
	if now.Before(leaf.NotBefore) {
		return fmt.Errorf("the certificate is not valid before %s", leaf.NotBefore.UTC().Format(dateFormat))
	}
	if now.After(leaf.NotAfter) {
		return fmt.Errorf("the certificate expired on %s", leaf.NotAfter.UTC().Format(dateFormat))
	}
	return nil
}

func checkNames(leaf *x509.Certificate, hosts []string) error {
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return fmt.Errorf("the certificate does not cover the host name %s", h)
		}
	}
	return nil
}

// verifyChain verifies leaf for server authentication with the self-signed certificates of chain and trusted as
// roots and the others as intermediates.
func verifyChain(leaf *x509.Certificate, chain []*x509.Certificate, trusted *x509.CertPool, now time.Time) error {
	roots := x509.NewCertPool()
	if trusted != nil {
		roots = trusted.Clone()
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain {
		if c.CheckSignatureFrom(c) == nil {
			roots.AddCert(c)
		} else {
			intermediates.AddCert(c)
		}
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	var invalid x509.CertificateInvalidError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &invalid) && invalid.Reason == x509.IncompatibleUsage:
		return errUsage
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return errors.New("a certificate of the chain has expired or is not valid yet")
	}
	return errChain
}
