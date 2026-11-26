package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filippo.io/age"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const pathUpdatesRollback = pathUpdates + "/rollback"

// rolledBack is a rollback the fake started: the kit's backup key and the user.
type rolledBack struct {
	id   age.Identity
	user string
}

func (f *fakePlatform) Rollback(_ context.Context, id age.Identity, user string) error {
	if f.rollbackErr != nil {
		return f.rollbackErr
	}
	f.rolledBack = append(f.rolledBack, rolledBack{id: id, user: user})
	return nil
}

func (f *fakePlatform) RollbackRefused(context.Context) string { return f.rollbackRefused }

func (h *harness) rollback(kit []byte, passphrase, confirm string, s *session) *httptest.ResponseRecorder {
	parts := map[string][]byte{"kit": kit, "passphrase": []byte(passphrase)}
	if confirm != "" {
		parts["confirm"] = []byte(confirm)
	}
	return h.upload(http.MethodPost, pathUpdatesRollback, parts, s)
}

// TestRollbackNeedsKit refuses a rollback without a confirmation or a kit that opens, starts one with the kit's
// backup key by the signed-in user, and never repeats the passphrase in an answer, the log or the audit log.
func TestRollbackNeedsKit(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	p := h.withPlatform()
	s := h.signIn()
	key, kit := restoreKit(t)

	var seen strings.Builder
	for _, c := range []struct {
		kit                 []byte
		passphrase, confirm string
		code                string
	}{
		{kit, restorePassphrase, "", codeInvalid},
		{kit, restorePassphrase, "false", codeInvalid},
		{nil, restorePassphrase, "true", codeInvalid},
		{kit, "not the passphrase at all", "true", "wrong_passphrase"},
		{[]byte("garbage"), restorePassphrase, "true", "invalid_kit"},
	} {
		rec := h.rollback(c.kit, c.passphrase, c.confirm, s)
		wantCode(t, rec, http.StatusBadRequest, c.code)
		seen.WriteString(rec.Body.String())
	}
	if len(p.rolledBack) != 0 {
		t.Fatalf("rolled back without a kit that opens: %+v", p.rolledBack)
	}
	if e := h.entry(update.AuditRollbackStart); e.Result != string(Failure) || e.User != testUser {
		t.Errorf("audit entry %+v", e)
	}

	wantStatus(t, h.rollback(kit, restorePassphrase, "true", s), http.StatusAccepted)
	if len(p.rolledBack) != 1 || p.rolledBack[0].user != testUser ||
		p.rolledBack[0].id.(*age.X25519Identity).String() != key.String() {
		t.Fatalf("rolled back %d times, want once by %s with the kit's key", len(p.rolledBack), testUser)
	}
	for _, e := range h.entries() {
		if e.Action == update.AuditRollbackStart && e.Result == string(Success) {
			t.Errorf("the API wrote the start's audit entry %+v", e)
		}
	}
	seen.WriteString(h.logs.String())
	fmt.Fprint(&seen, h.entries())
	if strings.Contains(seen.String(), restorePassphrase) || strings.Contains(seen.String(), key.String()) {
		t.Error("the passphrase or the backup key left the manager")
	}
}

// TestRollbackRefusals answers each refusal of the rollback with its code, records it, and answers a failure without
// its text.
func TestRollbackRefusals(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	p := h.withPlatform()
	s := h.signIn()
	_, kit := restoreKit(t)
	for _, c := range []struct {
		reason string
		code   string
	}{
		{update.NoPreUpdateBackup, "no_pre_update_backup"},
		{update.NoPreviousBundle, "no_previous_bundle"},
		{update.OperatorChanged, "operator_changed"},
		{update.NotNeeded, "not_needed"},
		{update.ExternalDatabase, "external_database"},
		{update.WrongKit, "foreign_kit"},
		{update.BackupBroken, "backup_broken"},
		{update.UpgradeInProgress, "upgrade_in_progress"},
		{update.UpdateRunning, "update_running"},
		{update.BackupRunning, "backup_running"},
		{update.RestoreRunning, "restore_running"},
		{update.BundleLoading, codeBundleLoading},
		{update.Busy, codeUpdateBusy},
		{update.NotInstalled, "not_installed"},
	} {
		p.rollbackErr = &update.Error{Reason: c.reason}
		wantCode(t, h.rollback(kit, restorePassphrase, "true", s), http.StatusConflict, c.code)
		if e := h.entry(update.AuditRollbackStart); e.Details[detailRefused] != c.code {
			t.Errorf("%s: audit entry %+v", c.reason, e)
		}
	}
	p.rollbackErr = &update.Error{Reason: update.NoPreviousBundle}
	if body := h.rollback(kit, restorePassphrase, "true", s).Body.String(); strings.Contains(strings.ToLower(body), "load it") ||
		!strings.Contains(body, "fresh appliance") {
		t.Errorf("no previous bundle answered %s, want the restore onto a fresh appliance and no load", body)
	}
	p.rollbackErr = fmt.Errorf("record the rollback: %w", errors.New("etcd says "+zotPassword))
	rec := h.rollback(kit, restorePassphrase, "true", s)
	wantCode(t, rec, http.StatusInternalServerError, "internal")
	if strings.Contains(rec.Body.String()+h.logs.String(), zotPassword) {
		t.Error("the rollback's error text left the manager")
	}
}

// TestRollbackUnavailable answers 503 without the update jobs.
func TestRollbackUnavailable(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	h.withUpdates()
	s := h.signIn()
	_, kit := restoreKit(t)
	wantCode(t, h.rollback(kit, restorePassphrase, "true", s), http.StatusServiceUnavailable, codeUpdatesUnavailable)
}

// TestUpdatesOfferRollback answers whether the last platform update can be rolled back, and why not.
func TestUpdatesOfferRollback(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	p := h.withPlatform()
	s := h.signIn()
	if u := updatesOf(t, h, s); u.Rollback == nil || !u.Rollback.Available || u.Rollback.Reason != nil {
		t.Errorf("rollback %+v, want available", u.Rollback)
	}
	p.rollbackRefused = update.OperatorChanged
	if u := updatesOf(t, h, s); u.Rollback == nil || u.Rollback.Available || u.Rollback.Reason == nil ||
		*u.Rollback.Reason != update.OperatorChanged {
		t.Errorf("rollback %+v, want refused for %s", u.Rollback, update.OperatorChanged)
	}
	h.srv.deps.Platform = nil
	if u := updatesOf(t, h, s); u.Rollback != nil {
		t.Errorf("rollback %+v without the update jobs", u.Rollback)
	}
}

// TestUpdatesShowTheRollback answers a rollback with the step and the failure of the restore that does it.
func TestUpdatesShowTheRollback(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	h.withPlatform()
	s := h.signIn()
	setStatus(t, func(st *v1alpha1.ApplianceStatus) {
		st.Update = &v1alpha1.UpdateStatus{Kind: update.KindRollback, From: "0.1.1", To: "0.1.0", Backup: "b-1", StartedAt: metav1.Now()}
		st.Restore = &v1alpha1.RestoreStatus{
			Backup: "b-1", InPlace: true, Step: v1alpha1.RestoreStepDatabase, Message: "timeout",
			Rollback: &v1alpha1.RestoreRollback{ILMVersion: "2.19.0", Reinstalled: true},
		}
	})
	u := updatesOf(t, h, s)
	if u.Update == nil || u.Update.Kind != UpdateProgressKindRollback || u.Update.Step == nil ||
		*u.Update.Step != v1alpha1.RestoreStepDatabase || u.Update.Message == nil || *u.Update.Message != "timeout" {
		t.Errorf("update %+v, want the rollback at the restore's step and failure", u.Update)
	}
}

// TestUpdatesNameTheRunningOperator answers the ILM operator version that runs.
func TestUpdatesNameTheRunningOperator(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	h.withPlatform()
	s := h.signIn()
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Versions.Operator = "v1.1.0" })
	if u := updatesOf(t, h, s); u.Operator == nil || *u.Operator != "v1.1.0" {
		t.Errorf("operator %v, want v1.1.0", u.Operator)
	}
}
