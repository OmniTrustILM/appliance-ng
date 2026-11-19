// Command drive runs the onboarding wizard of the appliance through its API for the boot test.
//
//	drive part1 --url <console> --code <code> --user <user> --password <password> --seed-file <file>
//	            --host-name <fqdn> --ca-file <pem> [--server-name <ip>] [--gateway <ip>] [--dns <ip>[,<ip>...]]
//	drive part2 --url <console> --user <user> --password <password> --seed-file <file>
//	            --host-name <fqdn> [--component <name>]
//	drive check --url <console> --user <user> --password <password> --seed-file <file>
//	            --host-name <fqdn> [--connector <display name> --ui-url <url> --ui-addr <host:port>]
//	drive api METHOD PATH --url <console> --user <user> --password <password> --seed-file <file>
//	            --host-name <fqdn> [--body <json>]
//
// part1 answers wizard part 1 of the first-boot service: the setup code, the administrator, the TOTP enrolment
// (the seed goes to --seed-file, mode 0600) and the node, with the address DHCP delivered as a static one and the
// gateway and DNS servers of the flags or else of DHCP, then applies. part2 signs in to the Appliance Manager and
// answers wizard part 2 with the host name and the component, if any.
// check signs in and checks that the setup is complete, the built-in certificate is served and the audit log holds
// the sign-in and the wizard. With --connector it downloads the ILM administrator's credential and checks, through
// the ILM Core API at --ui-url reached at --ui-addr, that the connector registered.
// api signs in to the Appliance Manager like part2, sends one request with the JSON body, if any, and prints the
// JSON answer as it is. The caller chooses a path whose answer holds no secret: not the ILM administrator's
// credential or a TOTP enrolment.
//
// Every connection is verified. part1 trusts the self-signed certificate of the first-boot service given with
// --ca-file for --server-name, the node address it is issued for. part2 and check wait until the console serves a
// certificate of the built-in CA for --host-name and then trust only the root CA of the built-in CA. drive prints
// step names and the outcome only, never the code, the password, the seed or a TOTP code: the boot test log is
// public.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"software.sslmate.com/src/go-pkcs12"
)

const (
	totpPeriod     = 30
	signInAttempts = 6
	pollInterval   = 5 * time.Second
	waitTimeout    = 3 * time.Minute
	// registerTimeout bounds the wait for the connector to start and register with Core after the setup.
	registerTimeout = 15 * time.Minute
	// certificateTimeout bounds the wait for the built-in CA to issue the console's certificate after hand-over.
	certificateTimeout = 15 * time.Minute
)

// slirpNodeAddress is the address QEMU's user network gives the node, which the certificate of the first-boot
// service is issued for.
const slirpNodeAddress = "10.0.2.15" // NOSONAR: fixed by QEMU's user network, used only by the boot test

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var o options
	fs.StringVar(&o.url, "url", "https://127.0.0.1:18443", "console URL")
	fs.StringVar(&o.code, "code", "", "setup code (part1)")
	fs.StringVar(&o.user, "user", "", "administrator user name")
	fs.StringVar(&o.password, "password", "", "administrator password")
	fs.StringVar(&o.seedFile, "seed-file", "", "file holding the TOTP seed")
	fs.StringVar(&o.hostName, "host-name", "", "host name of the appliance (part1, part2)")
	fs.StringVar(&o.gateway, "gateway", "", "default gateway of the static address (part1; default from DHCP)")
	fs.StringVar(&o.dns, "dns", "", "comma-separated DNS servers (part1; default from DHCP)")
	fs.StringVar(&o.component, "component", "", "component to install (part2)")
	fs.StringVar(&o.connector, "connector", "", "display name of a connector that must register with Core (check)")
	fs.StringVar(&o.uiURL, "ui-url", "", "base URL of the ILM (check)")
	fs.StringVar(&o.uiAddr, "ui-addr", "", "address to reach the ILM at, host:port (check)")
	fs.StringVar(&o.caFile, "ca-file", "", "PEM certificate of the first-boot service to trust (part1)")
	fs.StringVar(&o.serverName, "server-name", slirpNodeAddress,
		"node address the certificate of the first-boot service is issued for (part1)")
	fs.StringVar(&o.body, "body", "", "JSON body of the request (api)")
	args := os.Args[2:]
	if cmd == "api" {
		if len(args) < 2 {
			usage()
		}
		o.method, o.path, args = args[0], args[1], args[2:]
	}
	_ = fs.Parse(args)

	run, ok := map[string]func(context.Context, options) error{
		"part1": part1, "part2": part2, "check": check, "api": apiCall,
	}[cmd]
	if !ok {
		usage()
	}
	if err := o.validate(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "drive %s: %v\n", cmd, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := run(ctx, o); err != nil {
		fmt.Printf("drive %s: FAIL %v\n", cmd, err)
		os.Exit(1)
	}
	fmt.Printf("drive %s: OK\n", cmd)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: drive part1|part2|check --url <console> [flags]")
	fmt.Fprintln(os.Stderr, "       drive api METHOD PATH --url <console> [--body <json>] [flags]")
	os.Exit(2)
}

type options struct {
	url, code, user, password, seedFile, hostName, gateway, dns string
	component, connector, uiURL, uiAddr, caFile, serverName     string
	method, path, body                                          string
}

func (o options) validate(cmd string) error {
	missing := o.user == "" || o.password == "" || o.seedFile == ""
	switch {
	case missing:
		return errors.New("--user, --password and --seed-file are required")
	case cmd == "part1" && (o.code == "" || o.caFile == ""):
		return errors.New("--code and --ca-file are required")
	case o.hostName == "":
		return errors.New("--host-name is required")
	case o.connector != "" && (o.uiURL == "" || o.uiAddr == ""):
		return errors.New("--connector needs --ui-url and --ui-addr")
	case o.body != "" && !json.Valid([]byte(o.body)):
		return errors.New("--body is not valid JSON")
	}
	return nil
}

func step(name string) { fmt.Printf("drive: %s\n", name) }

// part1 answers wizard part 1 and applies it.
func part1(ctx context.Context, o options) error {
	roots, err := loadRoots(o.caFile)
	if err != nil {
		return err
	}
	c := newClient(o.url, roots, o.serverName)
	step("waiting for wizard part 1")
	if err := waitFor(ctx, func() error { return c.call(ctx, http.MethodGet, "/api/v1/setup/state", nil, nil) }); err != nil {
		return err
	}
	step("setup code")
	var tok struct{ CsrfToken string }
	if err := c.call(ctx, http.MethodPost, "/api/v1/setup/code", map[string]string{"code": o.code}, &tok); err != nil {
		return err
	}
	c.csrf = tok.CsrfToken
	step("administrator")
	admin := map[string]string{"user": o.user, "password": o.password}
	if err := c.call(ctx, http.MethodPut, "/api/v1/setup/admin", admin, nil); err != nil {
		return err
	}
	step("TOTP enrolment")
	var enrol struct{ Seed string }
	if err := c.call(ctx, http.MethodPost, "/api/v1/setup/totp", nil, &enrol); err != nil {
		return err
	}
	now := time.Now()
	code, err := totpCode(enrol.Seed, now)
	if err != nil {
		return err
	}
	step("TOTP confirmation")
	if err := c.call(ctx, http.MethodPost, "/api/v1/setup/totp/confirm", map[string]string{"code": code}, nil); err != nil {
		return err
	}
	if err := writeSeed(o.seedFile, enrol.Seed, now); err != nil {
		return err
	}
	step("node")
	node, err := c.nodeSettings(ctx, o)
	if err != nil {
		return err
	}
	if err := c.call(ctx, http.MethodPut, "/api/v1/setup/node", node, nil); err != nil {
		return err
	}
	step("apply")
	return c.call(ctx, http.MethodPost, "/api/v1/setup/apply", nil, nil)
}

// nodeSettings makes the address DHCP delivered static, with the gateway and DNS servers of o or else of DHCP, the
// Talos default time servers and no proxy.
func (c *client) nodeSettings(ctx context.Context, o options) (map[string]any, error) {
	var dhcp struct {
		Address, Gateway string
		DNS              []string `json:"dns"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/setup/dhcp", nil, &dhcp); err != nil {
		return nil, err
	}
	if o.gateway != "" {
		dhcp.Gateway = o.gateway
	}
	if o.dns != "" {
		dhcp.DNS = strings.Split(o.dns, ",")
	}
	if dhcp.Address == "" || dhcp.Gateway == "" || len(dhcp.DNS) == 0 {
		return nil, fmt.Errorf("no address (%q), gateway (%q) or DNS server (%q)", dhcp.Address, dhcp.Gateway, dhcp.DNS)
	}
	fmt.Printf("drive: static address %s gateway %s dns %s\n", dhcp.Address, dhcp.Gateway, strings.Join(dhcp.DNS, ","))
	return map[string]any{
		"hostName": o.hostName, "address": dhcp.Address, "gateway": dhcp.Gateway, "dns": dhcp.DNS, "ntp": []string{},
	}, nil
}

// part2 signs in to the Appliance Manager and answers wizard part 2.
func part2(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	step("wizard part 2")
	components := []string{}
	if o.component != "" {
		components = append(components, o.component)
	}
	platform := map[string]any{"hostName": o.hostName, "components": components}
	return c.call(ctx, http.MethodPut, "/api/v1/wizard/platform", platform, nil)
}

// check signs in and checks the outcome of the setup.
func check(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	step("status")
	var st struct {
		Setup      string
		Conditions []struct{ Type, Status string }
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
		return err
	}
	fmt.Printf("drive: setup %s\n", st.Setup)
	for _, cond := range st.Conditions {
		fmt.Printf("drive: %s=%s\n", cond.Type, cond.Status)
	}
	if st.Setup != "complete" {
		return fmt.Errorf("setup is %q, want complete", st.Setup)
	}
	for _, want := range []string{"HandoverComplete", "AddonsReady", "PlatformReady", "CertificateReady"} {
		if !slices.ContainsFunc(st.Conditions, func(c struct{ Type, Status string }) bool {
			return c.Type == want && c.Status == "True"
		}) {
			return fmt.Errorf("condition %s is not True", want)
		}
	}
	step("audit log")
	if err := c.checkAudit(ctx); err != nil {
		return err
	}
	step("console certificate")
	if err := waitFor(ctx, func() error { return c.servesBuiltinCertificate(ctx) }); err != nil {
		return err
	}
	if o.connector == "" {
		return nil
	}
	step("administrator credential")
	cert, err := c.adminCredential(ctx)
	if err != nil {
		return err
	}
	step("connector registration")
	ui, err := uiClient(ctx, c, o, cert)
	if err != nil {
		return err
	}
	return waitUntil(ctx, registerTimeout, func() error { return connectorRegistered(ctx, ui, o.uiURL, o.connector) })
}

// apiCall signs in to the Appliance Manager, sends the request of o and prints the answer.
func apiCall(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	step(o.method + " " + o.path)
	var body any
	if o.body != "" {
		body = json.RawMessage(o.body)
	}
	var answer []byte
	if err := c.call(ctx, o.method, o.path, body, &answer); err != nil {
		return err
	}
	if len(answer) > 0 {
		fmt.Println(string(answer))
	}
	return nil
}

// adminCredential downloads the ILM administrator's certificate and key under a random password, waiting until the
// platform issued them, and returns them as a TLS client certificate. The manager hands each certificate out once.
func (c *client) adminCredential(ctx context.Context) (tls.Certificate, error) {
	password := rand.Text()
	var pfx []byte
	var final error
	err := waitFor(ctx, func() error {
		body := map[string]string{"password": password}
		err := c.call(ctx, http.MethodPost, "/api/v1/platform/admin-credential", body, &pfx)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
			return err // not issued yet
		}
		final = err
		return nil
	})
	if err = errors.Join(err, final); err != nil {
		return tls.Certificate{}, err
	}
	key, leaf, chain, err := pkcs12.DecodeChain(pfx, password)
	if err != nil {
		return tls.Certificate{}, errors.New("the administrator credential does not decode")
	}
	cert := tls.Certificate{PrivateKey: key, Leaf: leaf, Certificate: [][]byte{leaf.Raw}}
	for _, ca := range chain {
		cert.Certificate = append(cert.Certificate, ca.Raw)
	}
	fmt.Printf("drive: administrator certificate %q issued by %q\n", leaf.Subject.CommonName, leaf.Issuer.CommonName)
	return cert, nil
}

// uiClient reaches the ILM at o.uiAddr whatever the URL's host. It verifies the ILM edge certificate against the root
// CA of the built-in CA for the host name of o.uiURL, and presents cert when the server asks for one.
func uiClient(ctx context.Context, c *client, o options, cert tls.Certificate) (*http.Client, error) {
	u, err := url.Parse(o.uiURL)
	if err != nil {
		return nil, fmt.Errorf("--ui-url: %w", err)
	}
	root, _, err := c.fetchRootCA(ctx)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	addr := o.uiAddr
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				ServerName: u.Hostname(),
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					return &cert, nil
				},
			},
		},
	}, nil
}

// connectorRegistered checks through the Core API at base that the connector with the display name is registered:
// connected, or waiting for approval.
func connectorRegistered(ctx context.Context, ui *http.Client, base, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/api/v1/connectors", nil)
	if err != nil {
		return err
	}
	res, err := ui.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /api/v1/connectors: HTTP %d", res.StatusCode)
	}
	var connectors []struct{ Name, Status string }
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&connectors); err != nil {
		return fmt.Errorf("GET /api/v1/connectors: decode the answer: %w", err)
	}
	i := slices.IndexFunc(connectors, func(c struct{ Name, Status string }) bool { return c.Name == name })
	if i < 0 {
		return fmt.Errorf("core lists no connector %q among %d", name, len(connectors))
	}
	status := connectors[i].Status
	if status != "connected" && status != "waitingForApproval" {
		return fmt.Errorf("connector %q has status %q", name, status)
	}
	fmt.Printf("drive: connector %q registered, status %s\n", name, status)
	return nil
}

// checkAudit checks that the audit log holds a successful sign-in and a successful wizard part 2.
func (c *client) checkAudit(ctx context.Context) error {
	var audit struct {
		Items []struct{ Action, Result string }
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/audit", nil, &audit); err != nil {
		return err
	}
	for _, action := range []string{"sign-in", "wizard-platform"} {
		if !slices.ContainsFunc(audit.Items, func(e struct{ Action, Result string }) bool {
			return e.Action == action && e.Result == "success"
		}) {
			return fmt.Errorf("audit log has no successful %s", action)
		}
	}
	fmt.Printf("drive: audit log holds %d entries\n", len(audit.Items))
	return nil
}

// fetchRootCA downloads the root CA of the built-in CA from the console and returns it with the TLS state of the
// connection.
func (c *client) fetchRootCA(ctx context.Context) (*x509.Certificate, *tls.ConnectionState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/ca.pem", nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		return nil, nil, errors.New("the console presented no certificate")
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET /api/v1/ca.pem: HTTP %d", res.StatusCode)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, nil, errors.New("ca.pem holds no certificate")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return root, res.TLS, nil
}

// servesBuiltinCertificate checks that the console's certificate chains to the root CA of the built-in CA.
func (c *client) servesBuiltinCertificate(ctx context.Context) error {
	root, state, err := c.fetchRootCA(ctx)
	if err != nil {
		return err
	}
	peers := state.PeerCertificates
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, cert := range peers[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := peers[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates}); err != nil {
		return fmt.Errorf("the console's certificate does not chain to the built-in CA: %w", err)
	}
	fmt.Printf("drive: console certificate issued by %q\n", peers[0].Issuer.CommonName)
	return nil
}

// managerClient waits until the console serves a certificate of the built-in CA for o.hostName and returns a
// client that verifies the console against the root CA of the built-in CA.
func managerClient(ctx context.Context, o options) (*client, error) {
	step("waiting for the built-in certificate")
	var c *client
	err := waitUntil(ctx, certificateTimeout, func() error {
		var err error
		c, err = trustBuiltinCA(ctx, o.url, o.hostName)
		return err
	})
	return c, err
}

// trustBuiltinCA returns a client of the console at base that trusts the root CA of the built-in CA for host. The
// root CA comes from the console itself, so it is trusted on first use: the certificate the console presents for
// host is pinned, the root CA is fetched over a connection verified against that pin and is accepted only when it
// issued the pinned certificate.
func trustBuiltinCA(ctx context.Context, base, host string) (*client, error) {
	presented, err := servedChain(ctx, base, host)
	if err != nil {
		return nil, err
	}
	pinned := x509.NewCertPool()
	pinned.AddCert(presented[0])
	root, _, err := newClient(base, pinned, host).fetchRootCA(ctx)
	if err != nil {
		return nil, err
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, cert := range presented[1:] {
		intermediates.AddCert(cert)
	}
	opts := x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: intermediates}
	if _, err := presented[0].Verify(opts); err != nil {
		return nil, fmt.Errorf("the console's certificate does not chain to the built-in CA: %w", err)
	}
	return newClient(base, roots, host), nil
}

// servedChain returns the certificates the console at base presents for host. They are not verified against any
// root: the caller decides whether to trust them.
func servedChain(ctx context.Context, base, host string) ([]*x509.Certificate, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	var unverified *tls.CertificateVerificationError
	switch {
	case errors.As(err, &unverified):
		// The handshake stops at the unknown issuer and hands over what the console presented.
		return unverified.UnverifiedCertificates, nil
	case err != nil:
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return conn.(*tls.Conn).ConnectionState().PeerCertificates, nil
}

// signIn signs in with the password and a TOTP code of a step not used before, which the manager would refuse as a
// replay. The seed file's modification time records the last step used. Failures are retried a few times, each
// with a fresh step, as the administrator may not be imported yet.
func (c *client) signIn(ctx context.Context, o options) error {
	step("waiting for the manager")
	if err := waitFor(ctx, func() error { return c.call(ctx, http.MethodGet, "/healthz", nil, nil) }); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		step("sign-in")
		err := c.signInOnce(ctx, o)
		if err == nil {
			return nil
		}
		if attempt == signInAttempts {
			return err
		}
		fmt.Printf("drive: sign-in failed (%v), retrying\n", err)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
			if err := sleep(ctx, time.Duration(apiErr.RetryAfter)*time.Second); err != nil {
				return err
			}
		}
	}
}

func (c *client) signInOnce(ctx context.Context, o options) error {
	seed, used, err := readSeed(o.seedFile)
	if err != nil {
		return err
	}
	if err := sleep(ctx, untilNextStep(used, time.Now())); err != nil {
		return err
	}
	now := time.Now()
	code, err := totpCode(seed, now)
	if err != nil {
		return err
	}
	if err := os.Chtimes(o.seedFile, now, now); err != nil {
		return err
	}
	var tok struct{ CsrfToken string }
	body := map[string]string{"user": o.user, "password": o.password, "totp": code}
	if err := c.call(ctx, http.MethodPost, "/api/v1/session", body, &tok); err != nil {
		return err
	}
	c.csrf = tok.CsrfToken
	return nil
}

// totpCode returns the TOTP code of seed at t as the appliance computes it: SHA-1, 6 digits, 30-second steps.
func totpCode(seed string, t time.Time) (string, error) {
	code, err := totp.GenerateCodeCustom(seed, t, totp.ValidateOpts{
		Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", errors.New("the TOTP seed is not valid base32")
	}
	return code, nil
}

// untilNextStep returns how long to wait at now until a TOTP step later than the one of used begins.
func untilNextStep(used, now time.Time) time.Duration {
	next := time.Unix((used.Unix()/totpPeriod+1)*totpPeriod, 0)
	return max(next.Sub(now), 0)
}

// writeSeed writes the seed to path, readable by the owner only, and records used as the last step used.
func writeSeed(path, seed string, used time.Time) error {
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return os.Chtimes(path, used, used)
}

func readSeed(path string) (seed string, used time.Time, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	return strings.TrimSpace(string(data)), info.ModTime(), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// waitFor calls f every pollInterval until it succeeds or waitTimeout passes, and returns its last error.
func waitFor(ctx context.Context, f func() error) error {
	return waitUntil(ctx, waitTimeout, f)
}

// waitUntil calls f every pollInterval until it succeeds or timeout passes, and returns its last error.
func waitUntil(ctx context.Context, timeout time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

type client struct {
	base string
	http *http.Client
	csrf string
}

// loadRoots returns a pool of the PEM certificates in the file at path.
func loadRoots(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return roots, nil
}

// newClient returns a client of the console at base that verifies the console's certificate against roots for
// serverName.
func newClient(base string, roots *x509.CertPool, serverName string) *client {
	jar, _ := cookiejar.New(nil) // never fails without options
	return &client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12},
			},
		},
	}
}

// apiError is a refused API call. It keeps the status and the error fields that never carry input values.
type apiError struct {
	method, path string
	status       int
	Code         string `json:"code"`
	Field        string `json:"field"`
	ServerTime   string `json:"serverTime"`
	RetryAfter   int    `json:"retryAfter"`
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.status)
	for _, kv := range [][2]string{{"code", e.Code}, {"field", e.Field}, {"serverTime", e.ServerTime}} {
		if kv[1] != "" {
			s += " " + kv[0] + "=" + kv[1]
		}
	}
	return s
}

// call sends body as JSON and decodes a 2xx answer into out when out is not nil, or stores it in out when out is a
// *[]byte. Any other answer is an apiError.
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" && method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		e := &apiError{method: method, path: path, status: res.StatusCode}
		_ = json.Unmarshal(data, e) // the fields stay empty for a body that is not an API error
		return e
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}
