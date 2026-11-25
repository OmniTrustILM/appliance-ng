package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

var testFiles = fstest.MapFS{
	"index.html":    {Data: []byte("<html>console</html>")},
	"assets/app.js": {Data: []byte("console.log(1)")},
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestConsoleServesIndexForUnknownPaths(t *testing.T) {
	h := Handler(testFiles)
	for _, path := range []string{"/", "/index.html", "/setup/part2", "/settings/network"} {
		res := get(t, h, path)
		if res.StatusCode != http.StatusOK || body(t, res) != "<html>console</html>" {
			t.Errorf("GET %s = %d, want the index page", path, res.StatusCode)
		}
	}
	res := get(t, h, "/assets/app.js")
	if res.StatusCode != http.StatusOK || body(t, res) != "console.log(1)" {
		t.Errorf("GET /assets/app.js = %d, want the file", res.StatusCode)
	}
}

func TestConsoleSecurityHeaders(t *testing.T) {
	h := Handler(testFiles)
	for _, path := range []string{"/", "/assets/app.js", "/api/x"} {
		res := get(t, h, path)
		for header, want := range map[string]string{
			"Content-Security-Policy": "default-src 'self'",
			"X-Frame-Options":         "DENY",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := res.Header.Get(header); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
}

func TestConsoleDoesNotServeAPIPaths(t *testing.T) {
	h := Handler(testFiles)
	for _, path := range []string{"/api/x", "/api/", "/api/v1/session"} {
		if res := get(t, h, path); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
}

func TestConsoleFilesHoldTheIndex(t *testing.T) {
	res := get(t, Handler(Files()), "/")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("GET / = %d %s, want the built index page", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestConsoleWithoutIndexFails(t *testing.T) {
	if res := get(t, Handler(fstest.MapFS{}), "/"); res.StatusCode != http.StatusInternalServerError {
		t.Errorf("GET / = %d, want 500 without index.html", res.StatusCode)
	}
}

func TestConsoleFilesPreferTheBuild(t *testing.T) {
	built := fstest.MapFS{
		"dist/index.html":     {Data: []byte("placeholder")},
		"dist/app/index.html": {Data: []byte("built")},
	}
	if got := body(t, get(t, Handler(files(built)), "/")); got != "built" {
		t.Errorf("GET / = %q, want the built console", got)
	}
	placeholder := fstest.MapFS{"dist/index.html": {Data: []byte("placeholder")}}
	if got := body(t, get(t, Handler(files(placeholder)), "/")); got != "placeholder" {
		t.Errorf("GET / = %q, want the placeholder without a build", got)
	}
}

func TestConsoleServesFaviconUnderBase(t *testing.T) {
	mux := http.NewServeMux()
	Mount(mux, fstest.MapFS{
		"index.html":  {Data: []byte("<html>console</html>")},
		"favicon.svg": {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
	})
	res := get(t, mux, "/appliance/favicon.svg")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/svg+xml" {
		t.Errorf("GET /appliance/favicon.svg = %d %s, want 200 image/svg+xml", res.StatusCode, res.Header.Get("Content-Type"))
	}
}
