package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestSelfSignedCertificateAddresses(t *testing.T) {
	cert, err := selfSignedCertificate(discard)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("certificate must cover 127.0.0.1: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("certificate must cover localhost: %v", err)
	}
	for _, ip := range leaf.IPAddresses {
		if ip.IsLinkLocalUnicast() {
			t.Errorf("certificate covers link-local address %s", ip)
		}
	}
	for _, ip := range hostAddresses() {
		if ip.To4() == nil || !ip.IsGlobalUnicast() {
			t.Errorf("host address %s is not a global unicast IPv4 address", ip)
		}
	}
}

// newCertificate returns the console certificate on an empty cluster, so it serves the self-signed one.
func newCertificate(t *testing.T) *Certificate {
	t.Helper()
	cert, err := NewCertificate(discard, fakeClient())
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestNewServerTimeouts(t *testing.T) {
	srv := NewServer(discard, "127.0.0.1:0", Routes(http.NotFoundHandler()), newCertificate(t))
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("timeouts %v/%v/%v/%v, want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
	if srv.TLSConfig.MinVersion < tls.VersionTLS12 {
		t.Errorf("minimum TLS version %x, want 1.2 or later", srv.TLSConfig.MinVersion)
	}
}

func TestRoutes(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Routes(api)
	if res := get(t, h, "/healthz"); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz status %d, want 200", res.StatusCode)
	}
	for _, path := range []string{"/appliance/setup", "/appliance/setup/", "/appliance/setup/admin"} {
		if res := get(t, h, path); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/appliance/signin" {
			t.Errorf("%s: status %d to %q, want 302 to /appliance/signin", path, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if res := get(t, h, "/appliance/setupx"); res.StatusCode != http.StatusOK {
		t.Errorf("/appliance/setupx status %d, want the console served", res.StatusCode)
	}
}

func TestConsoleServesUnderBase(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Routes(api)
	for _, path := range []string{"/appliance/", "/appliance/signin", "/appliance/no/such/page"} {
		if res := get(t, h, path); res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("GET %s = %d %s, want the console page", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	for _, path := range []string{"/appliance/api/v1/status", "/appliance/api/v1/ca.pem", "/appliance/api/x"} {
		if res := get(t, h, path); res.StatusCode != http.StatusTeapot {
			t.Errorf("GET %s = %d, want the API's answer", path, res.StatusCode)
		}
	}
	mux := http.NewServeMux()
	Mount(mux, testFiles)
	if res := get(t, mux, "/appliance/assets/app.js"); res.StatusCode != http.StatusOK || body(t, res) != "console.log(1)" {
		t.Errorf("GET /appliance/assets/app.js = %d, want the file", res.StatusCode)
	}
	if res := get(t, mux, "/appliance/api/v1/session"); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /appliance/api/v1/session = %d, want 404 from the console", res.StatusCode)
	}
}

func TestOldPathsRedirect(t *testing.T) {
	h := Routes(http.NotFoundHandler())
	for path, want := range map[string]string{
		"/":                       "/appliance/",
		"/setup/":                 "/appliance/setup/",
		"/signin":                 "/appliance/signin",
		"/api/v1/status":          "/appliance/api/v1/status",
		"/api/v1/ca.pem":          "/appliance/api/v1/ca.pem",
		"/api/v1/audit?limit=5":   "/appliance/api/v1/audit?limit=5",
		"/api/v1/setup/state?a=b": "/appliance/api/v1/setup/state?a=b",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != want {
				t.Errorf("%s %s = %d to %q, want 308 to %q", method, path, rec.Code, rec.Header().Get("Location"), want)
			}
		}
	}
}

func TestOldPathsRedirectStaysOnSite(t *testing.T) {
	for _, path := range []string{"//evil.example", "//evil.example/x", "/\\evil.example", "/%2F%2Fevil.example"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path, req.URL.RawPath = path, ""
		if strings.HasPrefix(path, "/%2F") {
			req.URL.Path, req.URL.RawPath = "///evil.example", path
		}
		req.Host = "evil.example"
		rec := httptest.NewRecorder()
		toBase().ServeHTTP(rec, req)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusPermanentRedirect || !strings.HasPrefix(loc, "/appliance/") || strings.HasPrefix(loc, "//") {
			t.Errorf("%s = %d to %q, want 308 under /appliance/", path, rec.Code, loc)
		}
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	srv := NewServer(discard, "127.0.0.1:0", Routes(http.NotFoundHandler()), newCertificate(t))
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, discard, srv, ln) }()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test server
	res, err := client.Get("https://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz status %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop after the context ended")
	}
}

func TestServeReportsBusyAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := NewServer(discard, ln.Addr().String(), Routes(http.NotFoundHandler()), newCertificate(t))
	if err := Serve(context.Background(), discard, srv); err == nil {
		t.Error("Serve on a used address must fail")
	}
}
