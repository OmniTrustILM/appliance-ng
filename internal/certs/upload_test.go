package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var (
	uploadNow   = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	uploadHosts = []string{"appliance.example.com", "ilm.example.com"}
	uploadIP    = netip.MustParseAddr("192.0.2.10")
)

// testIssuer is a CA certificate with its key.
type testIssuer struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func ecKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

var serial int64

// sign issues template for pub, signed by parent, or self-signed when parent is nil; it returns the certificate
// in DER.
func sign(t *testing.T, template *x509.Certificate, pub crypto.PublicKey, parent *testIssuer, self crypto.Signer) []byte {
	t.Helper()
	serial++
	template.SerialNumber = big.NewInt(serial)
	issuer, signer := template, self
	if parent != nil {
		issuer, signer = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// newIssuer returns a CA signed by parent, or a self-signed root when parent is nil.
func newIssuer(t *testing.T, name string, parent *testIssuer) (*testIssuer, []byte) {
	t.Helper()
	key := ecKey(t)
	template := &x509.Certificate{
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             uploadNow.Add(-24 * time.Hour),
		NotAfter:              uploadNow.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der := sign(t, template, key.Public(), parent, key)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testIssuer{cert: cert, key: key}, certPEM(der)
}

// leafTemplate is a server certificate for the upload's names and address, valid at uploadNow.
func leafTemplate() *x509.Certificate {
	return &x509.Certificate{
		Subject:     pkix.Name{CommonName: uploadHosts[0]},
		DNSNames:    uploadHosts,
		IPAddresses: []net.IP{net.IP(uploadIP.AsSlice())},
		NotBefore:   uploadNow.Add(-time.Hour),
		NotAfter:    uploadNow.Add(90 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// upload is the input of ValidateUpload.
type upload struct {
	cert, key, chain []byte
}

// chainFixture is a root, an intermediate it issued, and a valid upload of a leaf the intermediate issued.
type chainFixture struct {
	root, intermediate       *testIssuer
	rootPEM, intermediatePEM []byte
	valid                    upload
	leafKey                  *ecdsa.PrivateKey
}

func newChain(t *testing.T) chainFixture {
	t.Helper()
	var f chainFixture
	f.root, f.rootPEM = newIssuer(t, "Test Root", nil)
	f.intermediate, f.intermediatePEM = newIssuer(t, "Test Intermediate", f.root)
	f.leafKey = ecKey(t)
	f.valid = f.issue(t, leafTemplate(), f.leafKey)
	return f
}

// issue returns an upload of template for key, issued by the intermediate, with the whole chain.
func (f chainFixture) issue(t *testing.T, template *x509.Certificate, key crypto.Signer) upload {
	t.Helper()
	der := sign(t, template, key.Public(), f.intermediate, nil)
	return upload{cert: certPEM(der), key: keyPEM(t, key), chain: append(append([]byte{}, f.intermediatePEM...), f.rootPEM...)}
}

func TestUploadRejects(t *testing.T) {
	f := newChain(t)
	rsaSmall, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	with := func(change func(*x509.Certificate)) *x509.Certificate {
		tmpl := leafTemplate()
		change(tmpl)
		return tmpl
	}
	mismatch := f.valid
	mismatch.key = keyPEM(t, ecKey(t))
	noChain := f.valid
	noChain.chain = nil
	noIntermediate := f.valid
	noIntermediate.chain = f.rootPEM
	selfSignedKey := ecKey(t)
	selfSigned := upload{cert: certPEM(sign(t, leafTemplate(), selfSignedKey.Public(), nil, selfSignedKey)), key: keyPEM(t, selfSignedKey)}
	// openssl req -x509 marks the certificate as a CA; pasted into the chain too, it would be its own root.
	selfSignedCA := upload{cert: certPEM(sign(t, with(func(c *x509.Certificate) { c.IsCA, c.BasicConstraintsValid = true, true }),
		selfSignedKey.Public(), nil, selfSignedKey)), key: keyPEM(t, selfSignedKey)}
	selfSignedCA.chain = selfSignedCA.cert
	notPEM := f.valid
	notPEM.cert = []byte("not a certificate")
	keyAsCert := f.valid
	keyAsCert.cert = f.valid.key
	badKey := f.valid
	badKey.key = []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
	badChain := f.valid
	badChain.chain = append(append([]byte{}, f.intermediatePEM...), "trailing text"...)

	for _, tc := range []struct {
		name  string
		in    upload
		hosts []string
		want  string
	}{
		{"key mismatch", mismatch, uploadHosts, "the private key does not match the certificate"},
		{"expired", f.issue(t, with(func(c *x509.Certificate) {
			c.NotBefore, c.NotAfter = uploadNow.Add(-48*time.Hour), uploadNow.Add(-time.Hour)
		}), f.leafKey), uploadHosts, "the certificate expired on 2026-11-18 09:00 UTC"},
		{"not yet valid", f.issue(t, with(func(c *x509.Certificate) { c.NotBefore = uploadNow.Add(time.Hour) }), f.leafKey), uploadHosts,
			"the certificate is not valid before 2026-11-18 11:00 UTC"},
		{"chain does not verify", noChain, uploadHosts, "the chain does not lead to a trusted root certificate"},
		{"intermediate missing", noIntermediate, uploadHosts, "the chain does not lead to a trusted root certificate"},
		{"self-signed", selfSigned, uploadHosts, "the certificate is self-signed"},
		{"self-signed CA in its own chain", selfSignedCA, uploadHosts, "the certificate is self-signed"},
		{"host name missing", f.valid, append([]string{"pki.example.com"}, uploadHosts...), "the certificate does not cover the host name pki.example.com"},
		{"RSA 1024", f.issue(t, leafTemplate(), rsaSmall), uploadHosts, "the certificate's RSA key has 1024 bits; at least 2048 are needed"},
		{"Ed25519", f.issue(t, leafTemplate(), edKey), uploadHosts, "neither RSA nor ECDSA"},
		{"client certificate", f.issue(t, with(func(c *x509.Certificate) {
			c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}), f.leafKey), uploadHosts, "not for server authentication"},
		{"not PEM", notPEM, uploadHosts, "the certificate is not a PEM-encoded X.509 certificate"},
		{"key as certificate", keyAsCert, uploadHosts, "the certificate is not a PEM-encoded X.509 certificate"},
		{"unreadable key", badKey, uploadHosts, "the private key is not an unencrypted PEM-encoded RSA or ECDSA key"},
		{"chain with text", badChain, uploadHosts, "the chain is not a list of PEM-encoded X.509 certificates"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUpload(tc.in.cert, tc.in.key, tc.in.chain, tc.hosts, systemRoots(t), uploadNow)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want %q", err, tc.want)
			}
			if body := pemBody(tc.in.key); body != "" && strings.Contains(err.Error(), body) {
				t.Error("the error carries the key")
			}
		})
	}
}

func TestUploadRejectsAnExpiredIntermediate(t *testing.T) {
	f := newChain(t)
	key := ecKey(t)
	template := &x509.Certificate{
		Subject: pkix.Name{CommonName: "Old Intermediate"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		NotBefore: uploadNow.Add(-48 * time.Hour), NotAfter: uploadNow.Add(-time.Hour),
	}
	der := sign(t, template, key.Public(), f.root, nil)
	old, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f.intermediate, f.intermediatePEM = &testIssuer{cert: old, key: key}, certPEM(der)
	in := f.issue(t, leafTemplate(), f.leafKey)
	err = ValidateUpload(in.cert, in.key, in.chain, uploadHosts, systemRoots(t), uploadNow)
	if err == nil || !strings.Contains(err.Error(), "a certificate of the chain has expired") {
		t.Errorf("error %v, want an expired chain", err)
	}
}

func TestUploadAccepts(t *testing.T) {
	f := newChain(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaUpload := f.issue(t, leafTemplate(), rsaKey)
	// The chain follows the certificate in one file, and the key is PKCS #1.
	rsaUpload.cert = append(rsaUpload.cert, rsaUpload.chain...)
	rsaUpload.chain = nil
	rsaUpload.key = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})

	ecDER, err := x509.MarshalECPrivateKey(f.leafKey)
	if err != nil {
		t.Fatal(err)
	}
	sec1 := f.valid
	sec1.key = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER})
	// openssl ecparam -genkey writes the curve parameters before the key.
	withParams := f.valid
	withParams.key = append(pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}}), sec1.key...)

	// A certificate from a public CA does not carry the node's private address.
	noAddress := f.issue(t, func() *x509.Certificate { c := leafTemplate(); c.IPAddresses = nil; return c }(), f.leafKey)
	// The root is one of the appliance's trusted CAs and not part of the upload.
	trustedRoot := f.valid
	trustedRoot.chain = f.intermediatePEM
	withTrusted, err := TrustedRoots(f.rootPEM)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		in    upload
		roots *x509.CertPool
	}{
		"ECDSA with a separate chain":                     {f.valid, systemRoots(t)},
		"RSA 2048 with the chain in the certificate file": {rsaUpload, systemRoots(t)},
		"SEC 1 key after EC PARAMETERS":                   {withParams, systemRoots(t)},
		"SEC 1 key":                                       {sec1, systemRoots(t)},
		"no IP address":                                   {noAddress, systemRoots(t)},
		"root among the trusted CAs":                      {trustedRoot, withTrusted},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateUpload(tc.in.cert, tc.in.key, tc.in.chain, uploadHosts, tc.roots, uploadNow); err != nil {
				t.Errorf("refused: %v", err)
			}
			// The console and the edge load the stored files as they are.
			if _, err := tls.X509KeyPair(append(append([]byte{}, tc.in.cert...), tc.in.chain...), tc.in.key); err != nil {
				t.Errorf("an accepted upload does not load as a key pair: %v", err)
			}
		})
	}
}

func TestParsePrivateKeyRefusesNonSigners(t *testing.T) {
	// An X25519 key parses as PKCS #8 but cannot sign.
	der := []byte{0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x04, 0x22, 0x04, 0x20}
	der = append(der, make([]byte, 32)...)
	der[len(der)-1] = 1
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err == nil {
		t.Error("an X25519 key was accepted")
	}
	if _, err := ParsePrivateKey(nil); err == nil {
		t.Error("an empty key was accepted")
	}
}

// systemRoots are the system's root certificates without additional trusted CAs.
func systemRoots(t *testing.T) *x509.CertPool {
	t.Helper()
	roots, err := TrustedRoots(nil)
	if err != nil {
		t.Fatal(err)
	}
	return roots
}

// pemBody is the second line of the first PEM block, a distinctive part of its content.
func pemBody(p []byte) string {
	lines := strings.Split(strings.TrimSpace(string(p)), "\n")
	if len(lines) < 3 {
		return ""
	}
	return lines[1]
}
