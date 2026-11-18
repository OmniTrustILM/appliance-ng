package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

func testWizard(t *testing.T, ln net.Listener) (*Wizard, string) {
	t.Helper()
	code, display := NewCode(rand.Reader)
	return &Wizard{
		Code: code, Display: display, Version: "v1",
		Listen:  func() (net.Listener, error) { return ln, nil },
		Console: fstest.MapFS{"index.html": {Data: []byte("<html>console</html>")}},
		Network: func(context.Context, netip.Addr) (talos.Network, error) { return talos.Network{}, nil },
		Log:     slog.New(slog.DiscardHandler),
	}, display
}

func TestWizardRunServesTLSUntilCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w, display := testWizard(t, ln)
	lines := make(chan []string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := w.Run(ctx, netip.MustParseAddr("127.0.0.1"), func(l []string) { lines <- l })
		done <- err
	}()
	shown := <-lines
	if len(shown) != 4 || shown[3] != "Setup code "+display || !strings.HasPrefix(shown[2], "Certificate SHA-256 ") {
		t.Fatalf("console lines %q", shown)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/api/v1/setup/state")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	sum := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	if fp := strings.ReplaceAll(strings.TrimPrefix(shown[2], "Certificate SHA-256 "), ":", ""); !strings.EqualFold(fp, hex.EncodeToString(sum[:])) {
		t.Errorf("console fingerprint %s does not match the served certificate", fp)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"step":"code"`)) {
		t.Errorf("GET state = %d %s", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		_ = c.Close()
		t.Error("the setup port is still open after Run returned")
	}
}

func TestWizardRunFailsWithoutAListener(t *testing.T) {
	w, _ := testWizard(t, nil)
	w.Listen = func() (net.Listener, error) { return nil, errors.New("address in use") }
	if _, err := w.Run(context.Background(), netip.MustParseAddr("10.0.2.15"), func([]string) { t.Error("showed lines without a server") }); err == nil {
		t.Error("Run succeeded without a listener")
	}
}
