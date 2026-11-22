package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// fakeTalos is a node that counts reboots and shutdowns.
type fakeTalos struct {
	talos.Node
	mu        sync.Mutex
	reboots   int
	shutdowns int
	err       error
}

func (f *fakeTalos) Reboot(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reboots++
	return f.err
}

func (f *fakeTalos) Shutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns++
	return f.err
}

// withTalos gives the harness's server the node.
func (h *harness) withTalos(node talos.Node, err error) {
	h.srv.deps.Talos = func(context.Context) (talos.Node, error) { return node, err }
}

func TestRebootNeedsConfirm(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	node := &fakeTalos{}
	h.withTalos(node, nil)
	for _, path := range []string{"/appliance/api/v1/system/reboot", "/appliance/api/v1/system/shutdown"} {
		for _, body := range []any{map[string]bool{"confirm": false}, map[string]any{}, "null"} {
			rec := h.request(http.MethodPost, path, body, s)
			wantStatus(t, rec, http.StatusBadRequest)
			var got Error
			decode(t, rec, &got)
			if got.Field == nil || *got.Field != "confirm" {
				t.Errorf("%s %v: field %v, want confirm", path, body, got.Field)
			}
		}
	}
	if node.reboots != 0 || node.shutdowns != 0 {
		t.Fatalf("%d reboots and %d shutdowns without a confirmation", node.reboots, node.shutdowns)
	}

	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/system/reboot", Confirmation{Confirm: true}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/system/shutdown", Confirmation{Confirm: true}, s), http.StatusAccepted)
	if node.reboots != 1 || node.shutdowns != 1 {
		t.Fatalf("%d reboots and %d shutdowns, want one each", node.reboots, node.shutdowns)
	}
	entries := h.entries()
	for i, action := range []AuditEntryAction{AuditEntryActionReboot, AuditEntryActionShutdown} {
		e := entries[len(entries)-2+i]
		if e.Action != string(action) || e.Result != string(Success) || e.User != testUser {
			t.Errorf("audit entry %+v, want a successful %s by %s", e, action, testUser)
		}
	}
}

func TestRebootWithoutTalos(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	confirm := Confirmation{Confirm: true}
	// No Talos access configured, none yet, and a node that cannot be reached.
	wantUnavailable(t, h.request(http.MethodPost, "/appliance/api/v1/system/reboot", confirm, s))
	h.withTalos(nil, errors.New("no talosconfig yet"))
	wantUnavailable(t, h.request(http.MethodPost, "/appliance/api/v1/system/shutdown", confirm, s))
	node := &fakeTalos{err: errors.New("rpc error: code = Unavailable")}
	h.withTalos(node, nil)
	wantUnavailable(t, h.request(http.MethodPost, "/appliance/api/v1/system/reboot", confirm, s))
	wantUnavailable(t, h.request(http.MethodPost, "/appliance/api/v1/system/shutdown", confirm, s))
	entries := h.entries()
	if e := entries[len(entries)-1]; e.Action != string(AuditEntryActionShutdown) || e.Result != string(Failure) {
		t.Errorf("audit entry %+v, want a failed shutdown", e)
	}
}

func wantUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if body := rec.Body.String(); !strings.Contains(body, codeUnavailable) || strings.Contains(body, "rpc error") {
		t.Errorf("answer %s, want the fixed message only", body)
	}
}

func TestRebootRefusedWithoutAuditEntry(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	node := &fakeTalos{}
	h.withTalos(node, nil)
	h.srv.deps.Audit = failingAudit{}
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/system/reboot", Confirmation{Confirm: true}, s), http.StatusInternalServerError)
	if node.reboots != 0 {
		t.Error("the node rebooted without an audit entry")
	}
}
