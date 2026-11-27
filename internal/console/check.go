package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// checkTimeout bounds a check of the console.
const checkTimeout = 10 * time.Second

// errNoName is a served certificate without a DNS or IP name to verify it against.
var errNoName = errors.New("the served certificate names no DNS name or IP address")

// Check gets the console's health check at addr, host:port, over HTTPS, accepting nothing but the exact certificate
// the console serves: the answer comes from this console's listener, whatever CA issued its certificate and whether
// or not it expired.
func (c *Certificate) Check(ctx context.Context, addr string) error {
	tlsConfig, err := pinnedTLSConfig(c.served.Load().Certificate[0])
	if err != nil {
		return fmt.Errorf("check the console: %w", err)
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+healthPath, nil)
	if err != nil {
		return fmt.Errorf("check the console: %w", err)
	}
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("check the console: %w", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("check the console: status %d", res.StatusCode)
	}
	return nil
}

// pinnedTLSConfig returns a TLS client configuration that trusts the served leaf certificate alone: it is the only
// root, verified against a name it carries at a time within its validity, so an expired certificate still passes.
func pinnedTLSConfig(served []byte) (*tls.Config, error) {
	leaf, err := x509.ParseCertificate(served)
	if err != nil {
		return nil, fmt.Errorf("parse the served certificate: %w", err)
	}
	var name string
	switch {
	case len(leaf.DNSNames) > 0:
		name = leaf.DNSNames[0]
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		return nil, errNoName
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: name,
		Time:       func() time.Time { return leaf.NotBefore },
	}, nil
}
