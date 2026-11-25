package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	testUser     = "admin"
	testPassword = "correct horse battery staple"
)

// testClock is a clock tests move by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testServer struct {
	handler http.Handler
	wizard  *wizard
	display string
	clock   *testClock
	log     *bytes.Buffer
	applied chan machineconfig.Input
}

func testNetwork(context.Context) (talos.Network, error) {
	return talos.Network{
		Link:     "eth0",
		Address:  netip.MustParsePrefix("10.0.2.15/24"),
		Gateway:  netip.MustParseAddr("10.0.2.2"),
		DNS:      []netip.Addr{netip.MustParseAddr("10.0.2.3")},
		NTP:      []string{"pool.ntp.org"},
		HostName: "ilm.example.com",
	}, nil
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	code, display := NewCode(rand.Reader)
	s := &testServer{
		display: display,
		clock:   &testClock{t: time.Unix(1_800_000_000, 0)},
		log:     &bytes.Buffer{},
		applied: make(chan machineconfig.Input, 1),
	}
	s.wizard = newWizard(code, ServerDeps{
		Addr:    "127.0.0.1:0",
		Console: fstest.MapFS{"index.html": {Data: []byte("<html>console</html>")}},
		Network: testNetwork,
		Apply:   func(in machineconfig.Input) { s.applied <- in },
		Log:     slog.New(slog.NewTextHandler(s.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:     s.clock.now,
	})
	s.handler = s.wizard.handler()
	return s
}

// browser keeps the session cookie and CSRF token of one browser.
type browser struct {
	t      *testing.T
	s      *testServer
	cookie *http.Cookie
	csrf   string
}

func (s *testServer) browser(t *testing.T) *browser { return &browser{t: t, s: s} }

func (b *browser) do(method, path string, body any) *httptest.ResponseRecorder {
	b.t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.csrf != "" && method != http.MethodGet {
		req.Header.Set(auth.CSRFHeader, b.csrf)
	}
	rec := httptest.NewRecorder()
	b.s.handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			b.cookie = c
		}
	}
	return rec
}

func (b *browser) expect(method, path string, body any, want int) map[string]any {
	b.t.Helper()
	rec := b.do(method, path, body)
	if rec.Code != want {
		b.t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			b.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return out
}

func (b *browser) enterCode(code string) {
	b.t.Helper()
	out := b.expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": code}, http.StatusOK)
	b.csrf, _ = out["csrfToken"].(string)
	if b.csrf == "" || b.cookie == nil {
		b.t.Fatalf("no CSRF token or session cookie: %v", out)
	}
}

func (b *browser) step() string {
	b.t.Helper()
	step, _ := b.expect(http.MethodGet, "/appliance/api/v1/setup/state", nil, http.StatusOK)["step"].(string)
	return step
}

// chooseMode saves the choice between a new appliance and a restore.
func (b *browser) chooseMode(mode string) {
	b.t.Helper()
	b.expect(http.MethodPut, "/appliance/api/v1/setup/mode", map[string]string{"mode": mode}, http.StatusNoContent)
}

// enrol chooses a new appliance, sets the administrator and confirms a TOTP authenticator; it returns the seed.
func (b *browser) enrol() string {
	b.t.Helper()
	b.chooseMode("new")
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusNoContent)
	seed, _ := b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusOK)["seed"].(string)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": b.totp(seed)}, http.StatusNoContent)
	return seed
}

func (b *browser) totp(seed string) string {
	b.t.Helper()
	code, err := totp.GenerateCode(seed, b.s.clock.now())
	if err != nil {
		b.t.Fatal(err)
	}
	return code
}

func staticNode() map[string]any {
	return map[string]any{
		"hostName": "ilm.example.com",
		"address":  "192.0.2.10/24",
		"gateway":  "192.0.2.1",
		"dns":      []string{"192.0.2.53"},
		"ntp":      []string{"ntp.example.com", "192.0.2.123"},
		"proxy":    map[string]string{"http": "http://proxy.example.com:3128", "noProxy": "localhost,.example.com"},
	}
}

func TestSetupSingleSession(t *testing.T) {
	s := newTestServer(t)
	first := s.browser(t)
	if step := first.step(); step != "code" {
		t.Fatalf("step %q before the code, want code", step)
	}
	first.enterCode(s.display)
	first.enrol()

	second := s.browser(t)
	if rec := second.do(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": s.display}); rec.Code != http.StatusConflict {
		t.Fatalf("second browser with the code: %d, want 409", rec.Code)
	}
	if second.cookie != nil {
		t.Error("second browser got a session cookie")
	}
	if step := second.step(); step != "code" {
		t.Errorf("second browser sees step %q, want code", step)
	}
	if rec := second.do(http.MethodPut, "/appliance/api/v1/setup/node", staticNode()); rec.Code != http.StatusUnauthorized {
		t.Errorf("second browser without a session: %d, want 401", rec.Code)
	}

	reloaded := s.browser(t)
	reloaded.cookie = first.cookie
	out := reloaded.expect(http.MethodGet, "/appliance/api/v1/setup/state", nil, http.StatusOK)
	if out["step"] != "node" || out["csrfToken"] != first.csrf {
		t.Errorf("state after reload %v, want step node with the session's CSRF token", out)
	}
	// The same browser entering the code again keeps its session.
	reloaded.enterCode(s.display)
	if reloaded.csrf != first.csrf {
		t.Error("the owner entering the code again got a new session")
	}
}

func TestSetupTakeoverAfterTheSessionExpired(t *testing.T) {
	s := newTestServer(t)
	first := s.browser(t)
	first.enterCode(s.display)
	first.enrol()

	s.clock.advance(auth.IdleTimeout + time.Minute)
	second := s.browser(t)
	second.enterCode(s.display)
	if step := second.step(); step != "mode" {
		t.Errorf("step after a takeover %q, want mode: the answers start over", step)
	}
	if rec := first.do(http.MethodGet, "/appliance/api/v1/setup/dhcp", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired session: %d, want 401", rec.Code)
	}
}

func TestSetupStepsInOrder(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)

	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusConflict)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusConflict)
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusConflict)
	if step := b.step(); step != "mode" {
		t.Fatalf("step %q after the code, want mode", step)
	}
	b.chooseMode("new")
	b.chooseMode("new")
	if step := b.step(); step != "admin" {
		t.Fatalf("step %q after choosing the mode again, want admin", step)
	}
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusNoContent)
	if step := b.step(); step != "totp" {
		t.Fatalf("step %q, want totp", step)
	}
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusConflict)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": "123456"}, http.StatusConflict)

	old, _ := b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusOK)["seed"].(string)
	seed, _ := b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusOK)["seed"].(string)
	if seed == old {
		t.Fatal("a second enrolment returned the same seed")
	}
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusConflict)
	if rec := b.do(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": b.totp(old)}); rec.Code != http.StatusBadRequest {
		t.Errorf("code of the discarded seed: %d, want 400", rec.Code)
	}
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": b.totp(seed)}, http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusConflict)

	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusConflict)
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	if step := b.step(); step != "apply" {
		t.Fatalf("step %q, want apply", step)
	}
	// The review may go back to the node settings.
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)
	if step := b.step(); step != "applying" {
		t.Errorf("step %q, want applying", step)
	}
	if step := s.browser(t).step(); step != "applying" {
		t.Errorf("another browser sees step %q, want applying", step)
	}
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusConflict)
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusConflict)
	s.browser(t).expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": s.display}, http.StatusConflict)

	select {
	case <-s.applied:
	default:
		t.Fatal("apply did not hand the answers over")
	}
	select {
	case <-s.applied:
		t.Fatal("answers handed over twice")
	default:
	}
}

func TestSetupApplyHandsOverTheAnswers(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	seed := b.enrol()
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)

	in := <-s.applied
	if in.HostName != "ilm.example.com" || in.Address != netip.MustParsePrefix("192.0.2.10/24") ||
		in.Gateway != netip.MustParseAddr("192.0.2.1") || !slices.Equal(in.DNS, []netip.Addr{netip.MustParseAddr("192.0.2.53")}) ||
		!slices.Equal(in.NTP, []string{"ntp.example.com", "192.0.2.123"}) {
		t.Errorf("input %s %s %s %v %v", in.HostName, in.Address, in.Gateway, in.DNS, in.NTP)
	}
	if in.Proxy == nil || in.Proxy.HTTP != "http://proxy.example.com:3128" || in.Proxy.HTTPS != "" || in.Proxy.NoProxy != "localhost,.example.com" {
		t.Errorf("proxy %+v", in.Proxy)
	}
	var file HandoverFile
	if err := json.Unmarshal(in.Handover, &file); err != nil {
		t.Fatal(err)
	}
	if file.Version != HandoverVersion || file.Admin.User != testUser || file.Admin.TOTPSeed != seed ||
		!auth.CheckPassword(file.Admin.PasswordHash, testPassword) {
		t.Errorf("hand-over file version %d admin %q, seed or password hash wrong", file.Version, file.Admin.User)
	}
	want := NodeAnswers{
		HostName: "ilm.example.com", Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.53"},
		NTP: []string{"ntp.example.com", "192.0.2.123"}, Proxy: &Proxy{HTTP: "http://proxy.example.com:3128", NoProxy: "localhost,.example.com"},
	}
	got, _ := json.Marshal(file.Node)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(got, wantJSON) {
		t.Errorf("node answers %s, want %s", got, wantJSON)
	}
}

func TestSetupApplyHandsOverTheServedCertificate(t *testing.T) {
	s := newTestServer(t)
	cert, _, err := SelfSigned([]string{"10.0.2.15"})
	if err != nil {
		t.Fatal(err)
	}
	s.wizard.deps.Cert = cert
	b := s.browser(t)
	b.enterCode(s.display)
	b.enrol()
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)

	var file HandoverFile
	if err := json.Unmarshal((<-s.applied).Handover, &file); err != nil {
		t.Fatal(err)
	}
	if file.TLS == nil {
		t.Fatal("hand-over file carries no certificate")
	}
	served, err := tls.X509KeyPair([]byte(file.TLS.CertPEM), []byte(file.TLS.KeyPEM))
	if err != nil {
		t.Fatalf("hand-over certificate and key: %v", err)
	}
	if !bytes.Equal(served.Certificate[0], cert.Certificate[0]) {
		t.Error("hand-over certificate differs from the one the server served")
	}
}

func TestHandoverModeRestore(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.chooseMode("restore")
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusNoContent)
	seed, _ := b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusOK)["seed"].(string)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": b.totp(seed)}, http.StatusNoContent)
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)

	var file HandoverFile
	if err := json.Unmarshal((<-s.applied).Handover, &file); err != nil {
		t.Fatal(err)
	}
	if file.Mode != ModeRestore || file.Admin.User != testUser || file.Node.HostName != "ilm.example.com" {
		t.Errorf("hand-over file mode %q admin %q host %q, want restore with the answers", file.Mode, file.Admin.User, file.Node.HostName)
	}
}

func TestPutSetupModeAtLaterStepKeepsAnswers(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.enrol()
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.chooseMode("restore")
	if step := b.step(); step != "apply" {
		t.Fatalf("step %q after changing the mode at the review, want apply", step)
	}
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)

	var file HandoverFile
	if err := json.Unmarshal((<-s.applied).Handover, &file); err != nil {
		t.Fatal(err)
	}
	if file.Mode != ModeRestore || file.Admin.User != testUser || file.Node.HostName != "ilm.example.com" {
		t.Errorf("hand-over file mode %q admin %q host %q, want restore with the earlier answers", file.Mode, file.Admin.User, file.Node.HostName)
	}
}

func TestPutSetupModeAtModeAdvances(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.chooseMode("restore")
	if step := b.step(); step != "admin" {
		t.Errorf("step %q after the mode, want admin", step)
	}
}

func TestPutSetupModeWhileApplying(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.enrol()
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)
	b.expect(http.MethodPut, "/appliance/api/v1/setup/mode", map[string]string{"mode": "restore"}, http.StatusConflict)
}

func TestSetupTakeoverResetsTheMode(t *testing.T) {
	s := newTestServer(t)
	first := s.browser(t)
	first.enterCode(s.display)
	first.chooseMode("restore")

	s.clock.advance(auth.IdleTimeout + time.Minute)
	second := s.browser(t)
	second.enterCode(s.display)
	if step := second.step(); step != "mode" {
		t.Fatalf("step after a takeover %q, want mode", step)
	}
	if mode := s.wizard.mode; mode != "" {
		t.Errorf("mode %q survived the takeover, want it reset", mode)
	}
}

func TestSetupHandsOverNewMode(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.enrol()
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", staticNode(), http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)

	var file HandoverFile
	if err := json.Unmarshal((<-s.applied).Handover, &file); err != nil {
		t.Fatal(err)
	}
	if file.Mode != ModeNew {
		t.Errorf("hand-over file mode %q, want new", file.Mode)
	}
}

func TestSetupRefusesAnUnknownMode(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	for _, mode := range []string{"", "upgrade"} {
		b.expect(http.MethodPut, "/appliance/api/v1/setup/mode", map[string]string{"mode": mode}, http.StatusBadRequest)
	}
	if step := b.step(); step != "mode" {
		t.Errorf("step %q after refused modes, want mode", step)
	}
}

func TestSetupKeepDHCP(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.enrol()
	node := staticNode()
	node["address"], node["gateway"], node["ntp"] = "", "", []string{}
	delete(node, "proxy")
	b.expect(http.MethodPut, "/appliance/api/v1/setup/node", node, http.StatusNoContent)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/apply", nil, http.StatusAccepted)
	in := <-s.applied
	if in.Address.IsValid() || in.Gateway.IsValid() || in.Proxy != nil || len(in.NTP) != 0 {
		t.Errorf("DHCP input %+v %+v", in.Address, in.Proxy)
	}
}

func TestSetupValidatesTheNode(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.enrol()
	for name, tt := range map[string]struct {
		field string
		value any
	}{
		"single label":           {"hostName", "ilm"},
		"IP address host name":   {"hostName", "10.0.2.15"},
		"bad host name":          {"hostName", "ilm_1.example.com"},
		"address without prefix": {"address", "192.0.2.10"},
		"IPv6 address":           {"address", "2001:db8::10/64"},
		"link-local address":     {"address", "169.254.1.10/16"},
		"network address":        {"address", "192.0.2.0/24"},
		"no gateway":             {"gateway", ""},
		"gateway outside":        {"gateway", "198.51.100.1"},
		"gateway is the address": {"gateway", "192.0.2.10"},
		"no DNS":                 {"dns", []string{}},
		"bad DNS":                {"dns", []string{"dns.example.com"}},
		"link-local DNS":         {"dns", []string{"fe80::1"}},
		"bad NTP":                {"ntp", []string{"ntp example"}},
		"proxy scheme":           {"proxy", map[string]string{"http": "socks5://proxy.example.com"}},
		"proxy credentials":      {"proxy", map[string]string{"https": "http://user:secret@proxy.example.com"}},
		"proxy without host":     {"proxy", map[string]string{"http": "http://"}},
		"bad no-proxy":           {"proxy", map[string]string{"noProxy": "a b"}},
	} {
		node := staticNode()
		node[tt.field] = tt.value
		rec := b.do(http.MethodPut, "/appliance/api/v1/setup/node", node)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
			continue
		}
		var e struct{ Code, Field string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if wantField := strings.SplitN(tt.field, ".", 2)[0]; e.Field != wantField {
			t.Errorf("%s: field %q, want %q", name, e.Field, wantField)
		}
	}
	node := staticNode()
	node["address"] = ""
	if rec := b.do(http.MethodPut, "/appliance/api/v1/setup/node", node); rec.Code != http.StatusBadRequest {
		t.Errorf("gateway with DHCP: %d, want 400", rec.Code)
	}
	if step := b.step(); step != "node" {
		t.Errorf("step %q after refused settings, want node", step)
	}
}

// TestSetupBoundsConcurrentPasswordHashes checks that parallel administrator requests, which each need tens of MiB
// for the password hash, hash one at a time.
func TestSetupBoundsConcurrentPasswordHashes(t *testing.T) {
	s := newTestServer(t)
	var (
		mu           sync.Mutex
		active, peak int
	)
	s.wizard.hashPassword = func(string) (string, error) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return "hash", nil
	}
	b := s.browser(t)
	b.enterCode(s.display)
	b.chooseMode("new")
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			own := &browser{t: t, s: s, cookie: b.cookie, csrf: b.csrf}
			rec := own.do(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword})
			if rec.Code != http.StatusNoContent && rec.Code != http.StatusConflict {
				t.Errorf("status %d, want 204 or 409", rec.Code)
			}
		})
	}
	wg.Wait()
	if got := peak; got != maxPasswordHashes {
		t.Errorf("at most %d password hashes ran at once, want %d", got, maxPasswordHashes)
	}
}

func TestSetupValidatesTheAdmin(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.chooseMode("new")
	for name, body := range map[string]map[string]any{
		"no password":     {"user": testUser},
		"short password":  {"user": testUser, "password": "short"},
		"common password": {"user": testUser, "password": "unbelievable"},
		"no user":         {"user": "", "password": testPassword},
		"bad user":        {"user": "ad min", "password": testPassword},
	} {
		if rec := b.do(http.MethodPut, "/appliance/api/v1/setup/admin", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
}

func TestSetupCodeLockout(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	for range 4 {
		out := b.expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": "WRONG-CODE0"}, http.StatusUnauthorized)
		if _, locked := out["retryAfter"]; locked {
			t.Fatalf("locked before the fifth failure: %v", out)
		}
	}
	out := b.expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": "WRONG-CODE0"}, http.StatusUnauthorized)
	if out["retryAfter"] != float64(60) {
		t.Errorf("retryAfter %v, want 60", out["retryAfter"])
	}
	b.expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": s.display}, http.StatusUnauthorized)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": ""}, http.StatusBadRequest)
}

func TestSetupTOTPRefusalShowsServerTime(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.chooseMode("new")
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusNoContent)
	seed, _ := b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusOK)["seed"].(string)
	stale, err := totp.GenerateCode(seed, s.clock.now().Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	out := b.expect(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": stale}, http.StatusBadRequest)
	if out["serverTime"] != s.clock.now().UTC().Format(time.RFC3339) || out["code"] == "" {
		t.Errorf("refusal %v, want the code and the server time", out)
	}
	code := b.totp(seed)
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp/confirm", map[string]string{"code": code}, http.StatusNoContent)
}

func TestSetupRequiresCSRF(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.enterCode(s.display)
	b.chooseMode("new")
	csrf := b.csrf
	b.csrf = ""
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusForbidden)
	b.csrf = "wrong"
	b.expect(http.MethodPost, "/appliance/api/v1/setup/totp", nil, http.StatusForbidden)
	b.csrf = csrf
	b.expect(http.MethodPut, "/appliance/api/v1/setup/admin", map[string]string{"user": testUser, "password": testPassword}, http.StatusNoContent)
}

func TestSetupDHCP(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	b.expect(http.MethodGet, "/appliance/api/v1/setup/dhcp", nil, http.StatusUnauthorized)
	b.enterCode(s.display)
	out := b.expect(http.MethodGet, "/appliance/api/v1/setup/dhcp", nil, http.StatusOK)
	want := map[string]any{
		"hostName": "ilm.example.com", "address": "10.0.2.15/24", "gateway": "10.0.2.2",
		"dns": []any{"10.0.2.3"}, "ntp": []any{"pool.ntp.org"},
	}
	got, _ := json.Marshal(out)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(got, wantJSON) {
		t.Errorf("DHCP %s, want %s", got, wantJSON)
	}
}

func TestSetupDHCPUnavailable(t *testing.T) {
	code, display := NewCode(rand.Reader)
	srv := NewServer(code, ServerDeps{
		Console: fstest.MapFS{},
		Network: func(context.Context) (talos.Network, error) {
			return talos.Network{}, errors.New("machined unavailable")
		},
		Apply: func(machineconfig.Input) {},
		Log:   slog.New(slog.DiscardHandler),
	})
	b := (&testServer{handler: srv.Handler, clock: &testClock{t: time.Now()}}).browser(t)
	b.enterCode(display)
	if out := b.expect(http.MethodGet, "/appliance/api/v1/setup/dhcp", nil, http.StatusOK); len(out) != 0 {
		t.Errorf("DHCP %v, want nothing", out)
	}
}

func TestSetupRefusesMalformedRequests(t *testing.T) {
	s := newTestServer(t)
	b := s.browser(t)
	req := httptest.NewRequest(http.MethodPost, "/appliance/api/v1/setup/code", strings.NewReader(`{"code": "`+strings.Repeat("A", 64<<10)+`"}`))
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("oversized body: %d %s, want a JSON 400", rec.Code, rec.Header().Get("Content-Type"))
	}
	req = httptest.NewRequest(http.MethodPost, "/appliance/api/v1/setup/code", strings.NewReader(`{"code": SECRET-INPUT`))
	rec = httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "SECRET") {
		t.Errorf("malformed body: %d %s", rec.Code, rec.Body.String())
	}
	b.expect(http.MethodPost, "/appliance/api/v1/setup/code", map[string]string{"code": "A"}, http.StatusUnauthorized)
}

func TestSetupServesTheConsole(t *testing.T) {
	s := newTestServer(t)
	for path, want := range map[string]int{
		"/appliance/setup/":                  http.StatusOK,
		"/appliance/setup/node":              http.StatusOK,
		"/appliance/api/v1/session":          http.StatusNotFound,
		"/appliance/api/v1/appliance/status": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/appliance/api/v1/setup/state", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("API Cache-Control %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("API X-Content-Type-Options %q, want nosniff", got)
	}
}

func TestSetupOldPathsRedirect(t *testing.T) {
	s := newTestServer(t)
	for path, want := range map[string]string{
		"/":                       "/appliance/",
		"/setup/":                 "/appliance/setup/",
		"/api/v1/setup/state?x=1": "/appliance/api/v1/setup/state?x=1",
		"/api/v1/setup/code":      "/appliance/api/v1/setup/code",
	} {
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != want {
			t.Errorf("POST %s = %d to %q, want 308 to %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestNewServerSetsTLSAndTimeouts(t *testing.T) {
	cert, _, err := SelfSigned([]string{"10.0.2.15"})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := NewCode(rand.Reader)
	srv := NewServer(code, ServerDeps{Addr: ":8443", Cert: cert, Console: fstest.MapFS{}, Network: testNetwork,
		Apply: func(machineconfig.Input) {}, Log: slog.New(slog.DiscardHandler)})
	if srv.Addr != ":8443" || srv.TLSConfig == nil || len(srv.TLSConfig.Certificates) != 1 || srv.TLSConfig.MinVersion == 0 ||
		srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 || srv.ErrorLog == nil {
		t.Errorf("server %+v lacks TLS, timeouts or an error log", srv)
	}
}

func TestPasswordHashFreesItsSlotWhenItPanics(t *testing.T) {
	w := &wizard{hashSlots: auth.NewHashSlots(1), hashPassword: func(string) (string, error) { panic("hash failed") }}
	func() {
		defer func() { _ = recover() }()
		_, _ = w.hash(context.Background(), "password")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.hashSlots.Acquire(ctx); err != nil {
		t.Fatal("the hash slot was not freed")
	}
}
