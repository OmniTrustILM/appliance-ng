package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CSRSecretName is the Secret, in Namespace, that holds the private key of the pending signing request under
// corev1.TLSPrivateKeyKey.
const CSRSecretName = "appliance-tls-csr"

// maxCommonName is the longest common name X.509 allows; a longer first host name is in the DNS names only.
const maxCommonName = 64

// NewCSR generates an ECDSA P-256 key and a signing request for hosts, the first as the common name when it fits.
// It asks for no IP address, which public CAs do not sign for a private node address. It returns the request in PEM
// and the Secret CSRSecretName that keeps the key in the cluster; the signed certificate is uploaded against that key.
func NewCSR(hosts []string) ([]byte, *corev1.Secret, error) {
	if len(hosts) == 0 {
		return nil, nil, errors.New("a signing request needs at least one host name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate the key: %w", err)
	}
	template := &x509.CertificateRequest{DNSNames: hosts}
	if len(hosts[0]) <= maxCommonName {
		template.Subject = pkix.Name{CommonName: hosts[0]}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create the signing request: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the key: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: CSRSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), secret, nil
}
