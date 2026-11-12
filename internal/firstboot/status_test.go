package firstboot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusEndpoint(t *testing.T) {
	store := NewStatusStore()
	store.set(StateAddons, "keycloak", "waiting for Deployment keycloak/keycloak-operator: ImagePullBackOff")
	srv := httptest.NewServer(store.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status: %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "addons" || body["step"] != "keycloak" || body["message"] == "" {
		t.Errorf("body %v", body)
	}
	if _, err := time.Parse(time.RFC3339, body["since"]); err != nil {
		t.Errorf("since %q is not RFC 3339: %v", body["since"], err)
	}

	for _, path := range []string{"/", "/debug/requests", "/debug/pprof/", "/kubeconfig"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, resp.StatusCode)
		}
	}

	resp, err = http.Post(srv.URL+"/status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /status: %d, want 405", resp.StatusCode)
	}
}

func TestStatusSinceChangesWithStateOnly(t *testing.T) {
	store := NewStatusStore()
	store.set(StateAddons, "cilium", "applying")
	since := store.Get().Since
	time.Sleep(2 * time.Millisecond)
	store.set(StateAddons, "cilium", "waiting")
	if got := store.Get().Since; !got.Equal(since) {
		t.Errorf("Since moved on a message change: %v -> %v", since, got)
	}
	store.set(StateAddons, "storage", "applying")
	if got := store.Get().Since; !got.After(since) {
		t.Errorf("Since did not move on a step change: %v -> %v", since, got)
	}
}

func TestNewServerSetsTimeouts(t *testing.T) {
	srv := NewServer(":8443", NewStatusStore())
	if srv.Addr != ":8443" || srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("server %+v lacks address or timeouts", srv)
	}
}
