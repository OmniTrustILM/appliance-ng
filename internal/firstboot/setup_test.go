package firstboot

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime/extensions"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/setup"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const setupPassword = "correct horse battery staple"

// setupClient drives wizard part 1 over HTTPS like a browser.
type setupClient struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

func newSetupClient(t *testing.T, addr string) *setupClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &setupClient{t: t, base: "https://" + addr, client: &http.Client{
		Jar:       jar,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
}

func (c *setupClient) call(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+"/appliance/api/v1/setup/"+path, bytes.NewReader(data))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, c.csrf)
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d: %v", method, path, resp.StatusCode, want, out)
	}
	return out
}

// runWizard walks through wizard part 1 with a static address and returns the TOTP seed.
func (c *setupClient) runWizard(code string) string {
	c.t.Helper()
	c.csrf, _ = c.call(http.MethodPost, "code", map[string]string{"code": code}, http.StatusOK)["csrfToken"].(string)
	c.call(http.MethodPut, "admin", map[string]string{"user": "admin", "password": setupPassword}, http.StatusNoContent)
	seed, _ := c.call(http.MethodPost, "totp", nil, http.StatusOK)["seed"].(string)
	otp, err := totp.GenerateCode(seed, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	c.call(http.MethodPost, "totp/confirm", map[string]string{"code": otp}, http.StatusNoContent)
	if dhcp := c.call(http.MethodGet, "dhcp", nil, http.StatusOK); dhcp["address"] != "10.0.2.15/24" {
		c.t.Errorf("DHCP prefill %v", dhcp)
	}
	c.call(http.MethodPut, "node", map[string]any{
		"hostName": "ilm.example.com", "address": "10.0.2.20/24", "gateway": "10.0.2.2",
		"dns": []string{"10.0.2.3"}, "ntp": []string{"ntp.example.com"},
	}, http.StatusNoContent)
	c.call(http.MethodPost, "apply", nil, http.StatusAccepted)
	return seed
}

// setupHarness runs the service with the real wizard and machine configuration generator.
func setupHarness(t *testing.T) (h *harness, addr, display string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	node := &fakeNode{
		stage: talos.StageMaintenance, stageAfter: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr},
		kubeconfig: []byte("kubeconfig"),
		network:    talos.Network{Link: "eth0", Address: globalAddr[0], Gateway: netip.MustParseAddr("10.0.2.2")},
	}
	h = newHarness(node, newReadyCluster())
	code, display := setup.NewCode(rand.Reader)
	h.deps.Wizard = &setup.Wizard{
		Code: code, Display: display, Version: testVersion,
		Listen:  func() (net.Listener, error) { return ln, nil },
		Console: fstest.MapFS{"index.html": {Data: []byte("<html>console</html>")}},
		Network: node.Network,
		Log:     h.deps.Log,
	}
	generate := h.deps.Generate
	h.deps.Generate = func(in machineconfig.Input) ([]byte, error) {
		_, _ = generate(in)
		return machineconfig.Generate(in)
	}
	return h, ln.Addr().String(), display
}

func TestSetupApplyGeneratesConfig(t *testing.T) {
	h, addr, display := setupHarness(t)
	h.start(t)
	h.waitState(t, StateSetup)
	seed := newSetupClient(t, addr).runWizard(display)

	eventually(t, "apply", func() bool { return len(h.node.snapshot().applied) == 1 })
	cfg, err := configloader.NewFromBytes(h.node.snapshot().applied[0])
	if err != nil {
		t.Fatal(err)
	}
	var file setup.HandoverFile
	found := map[string]bool{}
	for _, doc := range cfg.Documents() {
		switch d := doc.(type) {
		case *network.HostnameConfigV1Alpha1:
			found["host name"] = d.ConfigHostname == "ilm.example.com"
		case *network.LinkConfigV1Alpha1:
			found["static address"] = d.MetaName == "eth0" && len(d.LinkAddresses) == 1 &&
				d.LinkAddresses[0].AddressAddress.String() == "10.0.2.20/24" &&
				len(d.LinkRoutes) == 1 && d.LinkRoutes[0].RouteGateway.String() == "10.0.2.2"
		case *network.ResolverConfigV1Alpha1:
			found["resolvers"] = len(d.ResolverNameservers) == 1 && d.ResolverNameservers[0].Address.String() == "10.0.2.3"
		case *network.TimeSyncConfigV1Alpha1:
			found["time servers"] = d.TimeNTP != nil && slices.Equal(d.TimeNTP.Servers, []string{"ntp.example.com"})
		case *extensions.ServiceConfigV1Alpha1:
			found["hand-over file"] = d.ServiceName == "firstboot" && len(d.ServiceConfigFiles) == 1 &&
				d.ServiceConfigFiles[0].ConfigFileMountPath == machineconfig.HandoverPath &&
				json.Unmarshal([]byte(d.ServiceConfigFiles[0].ConfigFileContent), &file) == nil
		}
	}
	for _, what := range []string{"host name", "static address", "resolvers", "time servers", "hand-over file"} {
		if !found[what] {
			t.Errorf("the configuration lacks the %s", what)
		}
	}
	if file.Admin.User != "admin" || file.Admin.TOTPSeed != seed || !auth.CheckPassword(file.Admin.PasswordHash, setupPassword) {
		t.Errorf("hand-over admin %q: seed or password hash wrong", file.Admin.User)
	}
	if !strings.Contains(string(h.node.snapshot().applied[0]), "https://10.0.2.20:6443") {
		t.Error("the cluster endpoint is not the static address")
	}
	if h.inputs[0].NodeIP != "10.0.2.20" || h.inputs[0].Link != "eth0" {
		t.Errorf("node IP %q link %q, want the static address on eth0", h.inputs[0].NodeIP, h.inputs[0].Link)
	}
	eventually(t, "installing line", func() bool {
		return strings.Contains(h.console.String(), "Installing, the console moves to https://10.0.2.20:8443/appliance/\n")
	})
	if c, err := net.Dial("tcp", addr); err == nil {
		_ = c.Close()
		t.Error("the setup port is still open after the apply")
	}
}

func TestSetupNeverLogsSecrets(t *testing.T) {
	h, addr, display := setupHarness(t)
	h.start(t)
	h.waitState(t, StateSetup)
	seed := newSetupClient(t, addr).runWizard(display)
	h.waitState(t, StateReady)
	h.stop(t)

	console, log, status := h.console.String(), h.log.String(), fmt.Sprint(h.store.Get())
	if !strings.Contains(console, "Setup code "+display+"\n") {
		t.Errorf("the console does not show the setup code:\n%s", console)
	}
	code := strings.ReplaceAll(display, "-", "")
	for name, out := range map[string]string{"log": log, "console": console, "status": status} {
		for what, secret := range map[string]string{"password": setupPassword, "seed": seed, "hash": "$argon2id$"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s contains the %s:\n%s", name, what, out)
			}
		}
		if name != "console" && (strings.Contains(out, display) || strings.Contains(out, code)) {
			t.Errorf("%s contains the setup code:\n%s", name, out)
		}
	}
}

func TestRunWaitsForTheWizard(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}}
	h := newHarness(node, nil)
	h.wizard.block = true
	h.start(t)

	h.waitState(t, StateSetup)
	want := strings.Join(wizardLines, "\n") + "\n"
	eventually(t, "wizard lines", func() bool { return strings.Contains(h.console.String(), want) })
	if strings.Contains(h.console.String(), "state setup") {
		t.Errorf("the console shows the status line during setup:\n%s", h.console.String())
	}
	if s := node.snapshot(); len(s.applied) != 0 {
		t.Errorf("applied %d configurations before the wizard finished", len(s.applied))
	}
}

func TestRunRunsTheWizardAgainWhenGenerateFails(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}}
	h := newHarness(node, nil)
	generate := h.deps.Generate
	failed := false
	h.deps.Generate = func(in machineconfig.Input) ([]byte, error) {
		if !failed {
			failed = true
			return nil, errors.New("invalid host name")
		}
		return generate(in)
	}
	h.start(t)

	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
	h.wizard.mu.Lock()
	defer h.wizard.mu.Unlock()
	if len(h.wizard.ips) != 2 {
		t.Errorf("wizard ran %d times, want 2", len(h.wizard.ips))
	}
}

func TestRunRetriesTheApplyWithTheSameAnswers(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr},
		applyErrs: []error{errors.New("machined unavailable")}}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
	h.wizard.mu.Lock()
	defer h.wizard.mu.Unlock()
	if len(h.wizard.ips) != 1 {
		t.Errorf("wizard ran %d times, want once", len(h.wizard.ips))
	}
}

func TestRunNeedsTheLinkOfAStaticAddress(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}, networkErr: errors.New("machined unavailable")}
	h := newHarness(node, nil)
	h.wizard.in.Address, h.wizard.in.Gateway = netip.MustParsePrefix("10.0.2.20/24"), netip.MustParseAddr("10.0.2.2")
	h.start(t)

	eventually(t, "error status", func() bool { return strings.Contains(h.store.Get().Message, "machined unavailable") })
	node.mu.Lock()
	node.networkErr = nil
	node.mu.Unlock()
	eventually(t, "error status", func() bool { return strings.Contains(h.store.Get().Message, "no link holds") })
	node.mu.Lock()
	node.network = talos.Network{Link: "eth0"}
	node.mu.Unlock()
	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
}
