package target

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	testAccessKey = "AKIAAPPLIANCETEST"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYsecretKEY"
)

func s3Creds() map[string][]byte {
	return map[string][]byte{"accessKey": []byte(testAccessKey), "secretKey": []byte(testSecretKey)}
}

// fakeS3 serves an in-memory S3 with the bucket ilm.
func fakeS3(t *testing.T, tlsServer bool) (*httptest.Server, *s3mem.Backend) {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("ilm"); err != nil {
		t.Fatal(err)
	}
	handler := gofakes3.New(backend).Server()
	var srv *httptest.Server
	if tlsServer {
		srv = httptest.NewTLSServer(handler)
	} else {
		srv = httptest.NewServer(handler)
	}
	t.Cleanup(srv.Close)
	return srv, backend
}

func s3Spec(endpoint, prefix string) v1alpha1.BackupTarget {
	return v1alpha1.BackupTarget{Name: "s3", Type: v1alpha1.BackupTypeS3, Retention: 3,
		S3: &v1alpha1.S3Target{Endpoint: endpoint, Bucket: "ilm", Prefix: prefix, Region: "us-east-1"}}
}

func TestS3RoundTrip(t *testing.T) {
	srv, backend := fakeS3(t, false)
	for _, size := range []int64{-1, 0} {
		target, err := New(s3Spec(srv.URL, "/appliance/backups/"), s3Creds(), Deps{})
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, target, size)
	}
	if _, err := backend.PutObject("ilm", "appliance/backups/x.ilmbackup", nil, strings.NewReader("x"), 1, nil); err != nil {
		t.Fatal(err)
	}
	target, err := New(s3Spec(srv.URL, ""), s3Creds(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if objects, err := target.List(context.Background()); err != nil || len(objects) != 0 {
		t.Errorf("List without the prefix = %+v, %v", objects, err)
	}
}

func TestS3UsesTheTargetCA(t *testing.T) {
	srv, _ := fakeS3(t, true)
	spec := s3Spec(srv.URL, "")
	plain, err := New(spec, s3Creds(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.List(context.Background()); err == nil {
		t.Fatal("an endpoint with an unknown CA was trusted")
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	for name, deps := range map[string]struct {
		creds map[string][]byte
		deps  Deps
	}{
		"target CA": {creds: withKey(s3Creds(), "ca.crt", ca)},
		"trust":     {creds: s3Creds(), deps: Deps{TrustPEM: ca}},
	} {
		target, err := New(spec, deps.creds, deps.deps)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.List(context.Background()); err != nil {
			t.Errorf("%s: List: %v", name, err)
		}
	}
	if _, err := New(spec, withKey(s3Creds(), "ca.crt", []byte("not a PEM")), Deps{}); err == nil {
		t.Error("a CA without certificates was accepted")
	}
}

func withKey(m map[string][]byte, key string, value []byte) map[string][]byte {
	m[key] = value
	return m
}

func TestS3ThroughTheProxy(t *testing.T) {
	srv, _ := fakeS3(t, false)
	backend, _ := url.Parse(srv.URL)
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.URL.Host)
		r.URL.Host, r.RequestURI = backend.Host, ""
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)
	// Loopback addresses are never proxied, so the target uses a name only the proxy resolves.
	spec := s3Spec("http://s3.example.test:9000", "")
	for name, tc := range map[string]struct {
		noProxy string
		via     bool
	}{
		"proxied":  {via: true},
		"no proxy": {noProxy: "example.test"},
	} {
		proxied = nil
		target, err := New(spec, s3Creds(), Deps{Proxy: &v1alpha1.ProxySpec{HTTP: proxy.URL, NoProxy: tc.noProxy}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = target.List(context.Background())
		if got := len(proxied) > 0 && proxied[0] == "s3.example.test:9000"; got != tc.via {
			t.Errorf("%s: requests through the proxy %v", name, proxied)
		}
	}
}

func TestS3BucketLookup(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"https://192.0.2.10:9000": true,
		"http://[2001:db8::1]":    true,
		"s3.example.com":          false,
		"https://s3.example.com":  false,
	} {
		target, err := New(s3Spec(endpoint, ""), s3Creds(), Deps{})
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		if got := target.(*s3Target).pathStyle; got != want {
			t.Errorf("%s: path style %v, want %v", endpoint, got, want)
		}
	}
	for _, endpoint := range []string{"ftp://s3.example.com", "https://s3.example.com/sub", "https://"} {
		if _, err := New(s3Spec(endpoint, ""), s3Creds(), Deps{}); err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
		}
	}
	if _, err := New(s3Spec("https://s3.example.com", ""), map[string][]byte{"accessKey": []byte("a")}, Deps{}); Reason(err) != ReasonAuthFailed {
		t.Errorf("credentials without a secret key: %v", err)
	}
}

func TestS3MissingBucketIsNotFound(t *testing.T) {
	srv, _ := fakeS3(t, false)
	spec := s3Spec(srv.URL, "")
	spec.S3.Bucket = "missing"
	target, err := New(spec, s3Creds(), Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1); Reason(err) != ReasonNotFound {
		t.Errorf("Put into a missing bucket: reason %q", Reason(err))
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonNotFound {
		t.Errorf("List of a missing bucket: reason %q", Reason(err))
	}
}
