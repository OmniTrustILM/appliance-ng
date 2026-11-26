package ilmclient

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testHost = "ilm.test"

// testCA is a CA of the tests.
type testCA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
	keyPEM  []byte
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	return newCA(t, cn, true)
}

// newCA returns a CA; without the CA flags its certificate has neither BasicConstraints CA nor the CertSign usage.
func newCA(t *testing.T, cn string, flags bool) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: flags, BasicConstraintsValid: flags, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if flags {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{
		cert: cert, key: key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

// issue returns a leaf of the CA for the DNS names, or a client certificate without names.
func (ca *testCA) issue(t *testing.T, cn string, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.ExtKeyUsageClientAuth
	if len(names) > 0 {
		usage = x509.ExtKeyUsageServerAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// fakeILM is Core behind the edge, which asks for a client certificate of the administrator CA, and Core reached
// directly as localhost. Users are found by the fingerprint of their certificate.
type fakeILM struct {
	t     *testing.T
	mu    sync.Mutex
	users map[string]*fakeUser // by UUID
	// registrations counts the calls of the bootstrap endpoint.
	registrations int
	// providers holds the stored OAuth2 providers by name.
	providers map[string]map[string]any
	// platform is the stored platform settings.
	platform map[string]any
	// requests records "METHOD path" of every edge request.
	requests []string
	// fail, when set, answers every edge request with this status and body.
	failStatus int
	failBody   string

	edge  *httptest.Server
	core  *httptest.Server
	roots *x509.CertPool
}

type fakeUser struct {
	uuid, username, fingerprint string
	// email and customAttributes are details an update has to send again: ILM takes the update's as the user's.
	email            *string
	customAttributes []map[string]any
}

func newFakeILM(t *testing.T, adminCA *testCA) *fakeILM {
	t.Helper()
	f := &fakeILM{t: t, users: map[string]*fakeUser{}, providers: map[string]map[string]any{}, platform: map[string]any{}}
	edgeCA := newTestCA(t, "edge-ca")
	f.roots = edgeCA.pool()
	f.edge = httptest.NewUnstartedServer(http.HandlerFunc(f.serveEdge))
	f.edge.TLS = &tls.Config{
		Certificates: []tls.Certificate{edgeCA.issue(t, testHost, testHost)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    adminCA.pool(),
		MinVersion:   tls.VersionTLS12,
	}
	f.edge.StartTLS()
	t.Cleanup(f.edge.Close)
	f.core = httptest.NewServer(http.HandlerFunc(f.serveCore))
	t.Cleanup(f.core.Close)
	return f
}

func (f *fakeILM) edgeAddr() string { return f.edge.Listener.Addr().String() }

// forward is the Forward of an Identity that reaches the fake Core.
func (f *fakeILM) forward(context.Context) (string, func(), error) {
	return f.core.URL, func() {}, nil
}

func (f *fakeILM) serveCore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/local/admins" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Username, Description, CertificateData string
		Enabled                                bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	der, err := base64.StdEncoding.DecodeString(body.CertificateData)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registrations++
	fp := Fingerprint(der)
	for _, u := range f.users {
		if u.fingerprint == fp {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `["Cannot assign certificate to the user because it is already assigned to other user"]`)
			return
		}
		if u.username == body.Username {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"Internal server error."}`)
			return
		}
	}
	u := &fakeUser{uuid: fmt.Sprintf("uuid-%d", len(f.users)+1), username: body.Username, fingerprint: fp}
	f.users[u.uuid] = u
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"uuid": u.uuid, "username": u.username, "certificate": map[string]any{"fingerprint": fp}})
}

// caller returns the user of the request's client certificate, or nil.
func (f *fakeILM) caller(r *http.Request) *fakeUser {
	fp := Fingerprint(r.TLS.PeerCertificates[0].Raw)
	for _, u := range f.users {
		if u.fingerprint == fp {
			return u
		}
	}
	return nil
}

func (f *fakeILM) serveEdge(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Host != testHost {
		f.t.Errorf("request for host %q, want %q", r.Host, testHost)
	}
	if f.failStatus != 0 {
		w.WriteHeader(f.failStatus)
		_, _ = io.WriteString(w, f.failBody)
		return
	}
	user := f.caller(r)
	if user == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"status":401,"error":"Unauthorized"}`)
		return
	}
	const providers = "/api/v1/settings/authentication/oauth2Providers/"
	switch {
	case r.URL.Path == "/api/v1/auth/profile":
		_ = json.NewEncoder(w).Encode(map[string]any{"username": user.username})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users":
		var list []map[string]any
		for _, u := range f.users {
			list = append(list, map[string]any{"uuid": u.uuid, "username": u.username})
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/users/"):
		f.getUser(w, strings.TrimPrefix(r.URL.Path, "/api/v1/users/"))
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/users/"):
		f.updateUser(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/users/"))
	case strings.HasPrefix(r.URL.Path, providers):
		f.serveProvider(w, r, strings.TrimPrefix(r.URL.Path, providers))
	case r.URL.Path == "/api/v1/settings/platform":
		f.servePlatform(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"No endpoint."}`)
	}
}

func (f *fakeILM) updateUser(w http.ResponseWriter, r *http.Request, uuid string) {
	u, ok := f.users[uuid]
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"Internal server error."}`)
		return
	}
	var body struct {
		Description, CertificateData string
		Email                        *string
		CustomAttributes             []map[string]any
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	der, err := base64.StdEncoding.DecodeString(body.CertificateData)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	u.fingerprint, u.email, u.customAttributes = Fingerprint(der), body.Email, body.CustomAttributes
	_ = json.NewEncoder(w).Encode(map[string]any{"uuid": u.uuid})
}

func (f *fakeILM) getUser(w http.ResponseWriter, uuid string) {
	u, ok := f.users[uuid]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uuid": u.uuid, "username": u.username, "email": u.email, "certificate": map[string]any{"fingerprint": u.fingerprint},
		"customAttributes": u.customAttributes,
	})
}

func (f *fakeILM) serveProvider(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		p, ok := f.providers[name]
		if !ok {
			return // 200 with an empty body, as ILM answers for a missing provider
		}
		out := map[string]any{"name": name}
		for k, v := range p {
			if k != "clientSecret" {
				out[k] = v
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodPut:
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.providers[name] = p
	case http.MethodDelete:
		delete(f.providers, name)
	}
}

func (f *fakeILM) servePlatform(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(f.platform)
	case http.MethodPut:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Each section sent is stored as sent; a section left out stays.
		for k, v := range body {
			f.platform[k] = v
		}
	}
}

func (f *fakeILM) registered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registrations
}

func (f *fakeILM) edgeRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}
