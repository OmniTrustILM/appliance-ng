package console

import (
	"bytes"
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

// errNotServed is a console that answers with another certificate than the one this console serves.
var errNotServed = errors.New("the console answered with another certificate than the one it serves")

// Check gets the console's health check at addr, host:port, over HTTPS, accepting nothing but the exact certificate
// the console serves: the answer comes from this console's listener, whatever CA issued its certificate and whether
// or not it expired.
func (c *Certificate) Check(ctx context.Context, addr string) error {
	served := c.served.Load().Certificate[0]
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The served certificate is pinned instead: VerifyPeerCertificate accepts it alone.
		InsecureSkipVerify: true, //nolint:gosec // the peer certificate is pinned below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || !bytes.Equal(raw[0], served) {
				return errNotServed
			}
			return nil
		},
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
