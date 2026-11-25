package update

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGuardRefusesRedirectsAwayFromHTTPS refuses to follow a redirect to http.
func TestGuardRefusesRedirectsAwayFromHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.RedirectHandler("http://example.com/v2/", http.StatusFound))
	t.Cleanup(srv.Close)
	c := &http.Client{Transport: guard(srv.Client().Transport.(*http.Transport).Clone(), idleTimeout)}
	resp, err := c.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect to http was followed")
	}
}

// TestGuardGivesUpAStalledAnswer fails an answer whose body stops arriving.
func TestGuardGivesUpAStalledAnswer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c := &http.Client{Transport: guard(srv.Client().Transport.(*http.Transport).Clone(), 200*time.Millisecond)}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	start := time.Now()
	buf := make([]byte, 200)
	for err == nil {
		_, err = resp.Body.Read(buf)
	}
	if time.Since(start) > 5*time.Second || reasonOf(err) != Timeout {
		t.Errorf("read ended after %v with %v (reason %s), want a timeout", time.Since(start), err, reasonOf(err))
	}
}
