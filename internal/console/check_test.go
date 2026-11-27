package console

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

// serveHealth serves the console's health check over TLS with the certificate of c and returns its address.
func serveHealth(t *testing.T, c *Certificate, status int) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	// httptest fills in its own certificate unless one is set, and a request to an address sends no server name.
	served, err := c.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*served}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

func TestCheckPinsTheServedCertificate(t *testing.T) {
	client := fakeClient(appliance(v1alpha1.TLSSpec{}))
	c, err := NewCertificate(discard, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background(), serveHealth(t, c, http.StatusOK)); err != nil {
		t.Errorf("check of the console: %v", err)
	}
	if err := c.Check(context.Background(), serveHealth(t, c, http.StatusServiceUnavailable)); err == nil {
		t.Error("an unhealthy console passed")
	}
	other, err := NewCertificate(discard, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background(), serveHealth(t, other, http.StatusOK)); err == nil {
		t.Error("a server with another certificate passed")
	}
}

// TestCheckPassesAnExpiredServedCertificate checks the console behind a certificate that expired: the check pins it,
// so an expired user certificate does not fail the health check of an update.
func TestCheckPassesAnExpiredServedCertificate(t *testing.T) {
	c, err := NewCertificate(discard, fakeClient(appliance(v1alpha1.TLSSpec{})))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := certs.SelfSigned("expired", []string{"console.example.com"}, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	c.served.Store(&expired)
	if err := c.Check(context.Background(), serveHealth(t, c, http.StatusOK)); err != nil {
		t.Errorf("check behind an expired served certificate: %v", err)
	}
}
