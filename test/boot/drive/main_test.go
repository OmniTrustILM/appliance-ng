package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/setup"
)

func TestTOTPMatchesAuth(t *testing.T) {
	seed, _, err := auth.NewTOTP("admin", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{time.Unix(0, 0), time.Unix(1_800_000_029, 0), time.Now()} {
		code, err := totpCode(seed, at)
		if err != nil {
			t.Fatalf("totpCode at %v: %v", at, err)
		}
		if !auth.CheckTOTP(seed, code, at) {
			t.Errorf("auth.CheckTOTP refuses the code drive generated at %v", at)
		}
	}
}

func TestNextStep(t *testing.T) {
	used := time.Unix(1_800_000_010, 0) // inside the step that starts at 1_800_000_000
	for _, tc := range []struct {
		now  time.Time
		want time.Duration
	}{
		{time.Unix(1_800_000_010, 0), 20 * time.Second},
		{time.Unix(1_800_000_029, 0), time.Second},
		{time.Unix(1_800_000_030, 0), 0},
		{time.Unix(1_800_000_100, 0), 0},
	} {
		if got := untilNextStep(used, tc.now); got != tc.want {
			t.Errorf("untilNextStep(%v) = %v, want %v", tc.now.Unix(), got, tc.want)
		}
	}
}

func TestServesBuiltinCertificateWithoutTLS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n"))
	}))
	defer srv.Close()
	err := newClient(srv.URL, nil, "").servesBuiltinCertificate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("servesBuiltinCertificate over plain HTTP = %v, want an error naming the missing certificate", err)
	}
}

func TestConnectorRegistered(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"connected":            {`[{"name":"X","status":"connected"}]`, true},
		"waiting for approval": {`[{"name":"X","status":"waitingForApproval"}]`, true},
		"offline":              {`[{"name":"X","status":"offline"}]`, false},
		"other connector":      {`[{"name":"Y","status":"connected"}]`, false},
		"none":                 {`[]`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/connectors" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		err := connectorRegistered(context.Background(), srv.Client(), srv.URL, "X")
		srv.Close()
		if (err == nil) != tc.want {
			t.Errorf("%s: connectorRegistered = %v, want registered %v", name, err, tc.want)
		}
	}
}

func TestValidateRequiresTheSetupCertificate(t *testing.T) {
	o := options{code: "c", user: "u", password: "p", seedFile: "s", hostName: "h"}
	if err := o.validate("part1"); err == nil || !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("validate part1 without --ca-file = %v, want an error naming it", err)
	}
	o.caFile = "ca.pem"
	if err := o.validate("part1"); err != nil {
		t.Errorf("validate part1 with --ca-file = %v", err)
	}
	if err := (options{user: "u", password: "p", seedFile: "s"}).validate("check"); err == nil {
		t.Error("validate check without --host-name passed")
	}
}

func TestValidateRequiresAJSONBody(t *testing.T) {
	o := options{user: "u", password: "p", seedFile: "s", hostName: "h", method: "PUT", path: "/appliance/api/v1/node", body: `{"dns":`}
	if err := o.validate("api"); err == nil || !strings.Contains(err.Error(), "--body") {
		t.Errorf("validate api with a broken body = %v, want an error naming it", err)
	}
	o.body = `{"dns":["192.0.2.53"]}`
	if err := o.validate("api"); err != nil {
		t.Errorf("validate api = %v", err)
	}
}

// TestPart1TrustsTheSetupCertificate checks that the self-signed certificate of the first-boot service, given as the
// only root, verifies for the node address it is issued for and for nothing else.
func TestPart1TrustsTheSetupCertificate(t *testing.T) {
	const nodeIP = "10.0.2.15"
	cert, _, err := setup.SelfSigned([]string{nodeIP})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "setup.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := loadRoots(caFile)
	if err != nil {
		t.Fatal(err)
	}
	err = newClient(srv.URL, roots, nodeIP).call(context.Background(), http.MethodGet, "/appliance/api/v1/setup/state", nil, nil)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusUnauthorized {
		t.Errorf("GET over the trusted setup certificate = %v, want HTTP 401", err)
	}
	err = newClient(srv.URL, roots, "10.0.2.16").call(context.Background(), http.MethodGet, "/appliance/api/v1/setup/state", nil, nil)
	if err == nil || errors.As(err, &apiErr) {
		t.Errorf("GET for another address = %v, want a TLS error", err)
	}
}

func TestLoadRootsRefusesAFileWithoutCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(path, []byte("none"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRoots(path); err == nil {
		t.Error("loadRoots of a file without a certificate passed")
	}
	if _, err := loadRoots(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("loadRoots of a missing file passed")
	}
}

// builtinCA returns a root CA and a server certificate it issued through an intermediate for hosts.
func builtinCA(t *testing.T, hosts ...string) (root *x509.Certificate, rootPEM []byte, served tls.Certificate) {
	t.Helper()
	issue := func(tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if parent == nil {
			parent, parentKey = tmpl, key
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	now := time.Now()
	ca := func(serial int64, name string) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour),
			NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		}
	}
	root, rootKey := issue(ca(1, "root"), nil, nil)
	inter, interKey := issue(ca(2, "intermediate"), root, rootKey)
	leaf, leafKey := issue(&x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "console"}, DNSNames: hosts,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, inter, interKey)
	served = tls.Certificate{Certificate: [][]byte{leaf.Raw, inter.Raw}, PrivateKey: leafKey, Leaf: leaf}
	return root, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}), served
}

// console serves cert and caPEM at /appliance/api/v1/ca.pem, or 404 when caPEM is nil.
func console(t *testing.T, cert tls.Certificate, caPEM []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/appliance/api/v1/ca.pem":
			_, _ = w.Write([]byte("ok"))
		case caPEM == nil:
			http.NotFound(w, r)
		default:
			_, _ = w.Write(caPEM)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestTrustBuiltinCA(t *testing.T) {
	const host = "ilm.appliance.internal"
	root, rootPEM, served := builtinCA(t, host)
	srv := console(t, served, rootPEM)
	c, err := trustBuiltinCA(context.Background(), srv.URL, host)
	if err != nil {
		t.Fatalf("trustBuiltinCA = %v", err)
	}
	if err := c.call(context.Background(), http.MethodGet, "/healthz", nil, nil); err != nil {
		t.Errorf("GET through the trusted client = %v", err)
	}
	if err := c.servesBuiltinCertificate(context.Background()); err != nil {
		t.Errorf("servesBuiltinCertificate = %v", err)
	}
	if !c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs.Equal(func() *x509.CertPool {
		p := x509.NewCertPool()
		p.AddCert(root)
		return p
	}()) {
		t.Error("the client does not trust exactly the root CA")
	}
}

func TestTrustBuiltinCARefuses(t *testing.T) {
	const host = "ilm.appliance.internal"
	_, rootPEM, served := builtinCA(t, host)
	_, otherPEM, _ := builtinCA(t, host)
	_, _, otherHost := builtinCA(t, "other.internal")
	self, _, err := setup.SelfSigned([]string{"10.0.2.15"})
	if err != nil {
		t.Fatal(err)
	}
	for name, srv := range map[string]*httptest.Server{
		"self-signed setup certificate": console(t, self, rootPEM),
		"certificate for another host":  console(t, otherHost, rootPEM),
		"root CA not issued yet":        console(t, served, nil),
		"root CA that did not issue it": console(t, served, otherPEM),
	} {
		if _, err := trustBuiltinCA(context.Background(), srv.URL, host); err == nil {
			t.Errorf("%s: trustBuiltinCA passed", name)
		}
	}
	if _, err := trustBuiltinCA(context.Background(), "https://127.0.0.1:1", host); err == nil {
		t.Error("trustBuiltinCA without a console passed")
	}
}

// TestPart2TrustsTheSetupCertificateUntilTheBuiltinOne checks that wizard part 2 reaches a console that still serves
// the setup certificate of --ca-file, and one that serves the built-in certificate already.
func TestPart2TrustsTheSetupCertificateUntilTheBuiltinOne(t *testing.T) {
	const host, nodeIP = "ilm.appliance.internal", "10.0.2.15"
	_, rootPEM, served := builtinCA(t, host)
	self, _, err := setup.SelfSigned([]string{nodeIP})
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "setup.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: self.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, srv := range map[string]*httptest.Server{
		"setup certificate":    console(t, self, rootPEM),
		"built-in certificate": console(t, served, rootPEM),
	} {
		o := options{url: srv.URL, caFile: caFile, serverName: nodeIP, hostName: host}
		c, err := part2Client(context.Background(), o)
		if err != nil {
			t.Errorf("%s: part2Client = %v", name, err)
			continue
		}
		if err := c.call(context.Background(), http.MethodGet, "/healthz", nil, nil); err != nil {
			t.Errorf("%s: GET through the trusted client = %v", name, err)
		}
	}
}

func TestDriveAPIPrintsNoAnswers(t *testing.T) {
	const answer = `{"seed":"ANSWER-MARKER"}`
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	o := options{method: http.MethodPut, path: "/appliance/api/v1/node", body: `{"dns":["192.0.2.53"]}`}
	var err error
	out := captureStdout(t, func() { err = newClient(srv.URL, nil, "").send(context.Background(), o) })
	if err != nil {
		t.Fatal(err)
	}
	if got != "PUT /appliance/api/v1/node" {
		t.Errorf("request %q, want PUT /appliance/api/v1/node", got)
	}
	if strings.Contains(out, "ANSWER-MARKER") || strings.TrimSpace(out) != "drive: PUT /appliance/api/v1/node" {
		t.Errorf("output %q, want the step name only", out)
	}
}
