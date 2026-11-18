package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

// Certificate is the console's certificate: the TLS Secret the Appliance selects once it exists, a self-signed one
// before. Refresh picks up a renewed or replaced Secret, so the console needs no restart. It is safe for
// concurrent use.
type Certificate struct {
	log    *slog.Logger
	reader client.Reader

	served atomic.Pointer[tls.Certificate]

	mu sync.Mutex
	// loaded is the certificate and key last loaded from a Secret.
	loaded []byte
}

// NewCertificate returns the console certificate that reads the Appliance and its TLS Secret with reader, serving
// a new self-signed certificate until the first Refresh finds the Secret.
func NewCertificate(log *slog.Logger, reader client.Reader) (*Certificate, error) {
	self, err := selfSignedCertificate(log)
	if err != nil {
		return nil, err
	}
	c := &Certificate{log: log, reader: reader}
	c.served.Store(&self)
	return c, nil
}

// GetCertificate returns the certificate to serve, for tls.Config.GetCertificate.
func (c *Certificate) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.served.Load(), nil
}

// Refresh loads the TLS Secret the Appliance selects: appliance-tls for the built-in certificate, tls.secretRef
// for source secret. Until the Appliance or the Secret exists, and when the Secret does not hold a valid pair,
// the certificate served so far stays.
func (c *Certificate) Refresh(ctx context.Context) error {
	a := &v1alpha1.Appliance{}
	err := c.reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("read the Appliance: %w", err)
	}
	name := certs.SecretName
	if a.Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		name = a.Spec.TLS.SecretRef
	}
	secret := &corev1.Secret{}
	err = c.reader.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("read secret %s/%s: %w", certs.Namespace, name, err)
	}

	certPEM, keyPEM := secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]
	pair := bytes.Join([][]byte{certPEM, keyPEM}, []byte{0})
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes.Equal(pair, c.loaded) {
		return nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// The parse error could quote key material, so it is not passed on.
		return fmt.Errorf("secret %s/%s does not hold a valid certificate and key", certs.Namespace, name)
	}
	c.served.Store(&cert)
	c.loaded = pair
	c.log.Info("console certificate loaded", "secret", certs.Namespace+"/"+name, "notAfter", cert.Leaf.NotAfter)
	return nil
}

// Run refreshes the certificate now and then every interval until ctx ends; failures are logged and retried.
func (c *Certificate) Run(ctx context.Context, every time.Duration) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := c.Refresh(ctx); err != nil {
			c.log.Warn("the console certificate was not refreshed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
