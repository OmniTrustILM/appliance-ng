package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	pathRecoveryKit = "/appliance/api/v1/recovery-kit"
	pathReissue     = pathRecoveryKit + "/reissue"
	pathReplace     = pathRecoveryKit + "/replace"
	pathConfirm     = pathRecoveryKit + "/confirm"

	kitPassphrase  = "kit-passphrase-that-must-stay-inside"
	nextPassphrase = "next-kit-passphrase-that-must-stay-inside"
)

// withKitAccess prepares a set-up Appliance whose node issues talosconfigs and whose token controller runs.
func withKitAccess(t *testing.T, h *harness) {
	t.Helper()
	withAccess(t)
	fillAccessTokens(t)
	h.withTalos(&talosNode{}, nil)
}

// openKit opens the kit an answer carries with passphrase.
func openKit(t *testing.T, rec *httptest.ResponseRecorder, passphrase string) recovery.Kit {
	t.Helper()
	wantStatus(t, rec, http.StatusOK)
	kit, err := recovery.Open(rec.Body.Bytes(), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	return kit
}

// issueKit issues the first kit and returns its file.
func issueKit(t *testing.T, h *harness, s *session) []byte {
	t.Helper()
	rec := h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s)
	wantStatus(t, rec, http.StatusOK)
	return rec.Body.Bytes()
}

func reissue(h *harness, s *session, kit []byte, passphrase, next string) *httptest.ResponseRecorder {
	return h.upload(http.MethodPost, pathReissue, map[string][]byte{"kit": kit, "passphrase": []byte(passphrase), "newPassphrase": []byte(next)}, s)
}

func kitState(t *testing.T, h *harness, s *session) RecoveryKit {
	t.Helper()
	rec := h.request(http.MethodGet, pathRecoveryKit, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var k RecoveryKit
	decode(t, rec, &k)
	return k
}

func storedRecipient(t *testing.T) string {
	t.Helper()
	return string(systemSecret(t, recovery.RecipientSecret).Data[recovery.RecipientKey])
}

func storedIssuance(t *testing.T) string {
	t.Helper()
	return string(systemSecret(t, recovery.RecipientSecret).Data[recovery.IssuanceKey])
}

func confirm(h *harness, s *session, issuance string) *httptest.ResponseRecorder {
	return h.request(http.MethodPost, pathConfirm, RecoveryKitConfirm{Issuance: issuance}, s)
}

// confirmStored confirms the kit of the stored issuance, or of an unknown one while no kit was issued.
func confirmStored(t *testing.T, h *harness, s *session) *httptest.ResponseRecorder {
	t.Helper()
	issuance := "unknown"
	if _, err := recovery.StoredRecipient(context.Background(), testClient); err == nil {
		issuance = storedIssuance(t)
	}
	return confirm(h, s, issuance)
}

func TestConfirmOnlyTheDownloadedKit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	rec := h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s)
	wantStatus(t, rec, http.StatusOK)
	first := rec.Header().Get("X-Recovery-Kit-Issuance")
	if first == "" || first != storedIssuance(t) || strings.Contains(first, storedRecipient(t)) {
		t.Fatalf("issuance %q, stored %q", first, storedIssuance(t))
	}
	if k := kitState(t, h, s); k.Issuance == nil || *k.Issuance != first {
		t.Errorf("kit %+v, want issuance %s", k, first)
	}

	rec = reissue(h, s, rec.Body.Bytes(), kitPassphrase, nextPassphrase)
	wantStatus(t, rec, http.StatusOK)
	second := rec.Header().Get("X-Recovery-Kit-Issuance")
	if second == "" || second == first || second != storedIssuance(t) {
		t.Fatalf("issuance after reissue %q, first %q, stored %q", second, first, storedIssuance(t))
	}

	rec = confirm(h, s, first)
	wantStatus(t, rec, http.StatusConflict)
	var e Error
	decode(t, rec, &e)
	if e.Code != "kit_superseded" {
		t.Errorf("confirm of the earlier kit refused with %s, want kit_superseded", e.Code)
	}
	if k := kitState(t, h, s); k.Confirmed || k.Issuance == nil || *k.Issuance != second {
		t.Errorf("kit after a stale confirm %+v", k)
	}
	wantStatus(t, h.request(http.MethodPost, pathConfirm, nil, s), http.StatusBadRequest)

	wantStatus(t, confirm(h, s, second), http.StatusNoContent)
	if k := kitState(t, h, s); !k.Confirmed {
		t.Errorf("kit after confirm %+v", k)
	}
}

func TestIssueAndConfirmTheKit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	if k := kitState(t, h, s); k.Issued || k.Confirmed {
		t.Fatalf("kit before issue %+v", k)
	}
	rec := h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr("short")}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	var e Error
	decode(t, rec, &e)
	if e.Field == nil || *e.Field != "passphrase" {
		t.Errorf("refusal %+v", e)
	}

	rec = h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s)
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="ilm-appliance-recovery-kit.tar.age"`) {
		t.Errorf("Content-Disposition %q", got)
	}
	kit := openKit(t, rec, kitPassphrase)
	kubeconfig := httptest.NewRecorder()
	kubeconfig.Body.Write(kit.Kubeconfig)
	if kubeconfigToken(t, kubeconfig) != longLivedToken {
		t.Error("the kit's kubeconfig does not carry the break-glass token")
	}
	cfg, err := clientconfig.FromBytes(kit.Talosconfig)
	if err != nil {
		t.Fatal(err)
	}
	if endpoints := cfg.Contexts[cfg.Context].Endpoints; len(endpoints) != 1 || endpoints[0] != certNodeAddress {
		t.Errorf("talosconfig endpoints %v", endpoints)
	}
	want := recovery.Identity{HostName: "appliance.example.com", Address: certNodeAddress}
	if kit.Identity != want {
		t.Errorf("identity %+v, want %+v", kit.Identity, want)
	}
	recipient := kit.BackupKey.Recipient().String()
	if storedRecipient(t) != recipient {
		t.Error("the stored recipient is not the kit's")
	}
	st := getAppliance(t).Status.RecoveryKit
	if st == nil || st.Confirmed || st.Recipient != recipient || st.IssuedAt == nil || st.TalosconfigNotAfter == nil {
		t.Fatalf("status %+v", st)
	}
	if notAfter := st.TalosconfigNotAfter.Time; notAfter.Before(time.Now().Add(364 * 24 * time.Hour)) {
		t.Errorf("talosconfig expires %v", notAfter)
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRecoveryKitIssue) || e.Result != string(Success) || e.Details["recipient"] != recipient {
		t.Errorf("audit entry %+v", e)
	}
	if k := kitState(t, h, s); !k.Issued || k.Confirmed || k.Recipient == nil || *k.Recipient != recipient || k.TalosconfigNotAfter == nil {
		t.Errorf("kit after issue %+v", k)
	}

	wantStatus(t, h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s), http.StatusConflict)
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)
	if e := lastEntry(h); e.Action != string(AuditEntryActionRecoveryKitConfirm) || e.Result != string(Success) {
		t.Errorf("audit entry %+v", e)
	}
	if k := kitState(t, h, s); !k.Confirmed {
		t.Error("the kit is not confirmed")
	}
	statusRec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
	wantStatus(t, statusRec, http.StatusOK)
	var status Status
	decode(t, statusRec, &status)
	if status.RecoveryKit == nil || !status.RecoveryKit.Confirmed || status.RecoveryKit.TalosconfigNotAfter == nil {
		t.Errorf("status recovery kit %+v", status.RecoveryKit)
	}
}

// fakeBackups records the backups the API starts.
type fakeBackups struct {
	BackupRunner
	runs chan []string
	err  error
}

func (f *fakeBackups) Run(_ context.Context, reason string, targets []string, opts ...backup.RunOption) (v1alpha1.BackupRecord, error) {
	f.runs <- append([]string{reason, fmt.Sprint(len(opts))}, targets...)
	return v1alpha1.BackupRecord{}, f.err
}

func TestConfirmStartsABackup(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	a := getAppliance(t)
	a.Spec.Backup = &v1alpha1.BackupSpec{Targets: []v1alpha1.BackupTarget{
		{Name: "s3", Type: v1alpha1.BackupTypeS3, Retention: 14, S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "b"}},
		{Name: "nfs", Type: v1alpha1.BackupTypeNFS, Retention: 14, NFS: &v1alpha1.NFSTarget{Server: "nfs.example.com", Export: "/e"}},
	}}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	runner := &fakeBackups{runs: make(chan []string, 1), err: backup.ErrBusy}
	h.srv.deps.Backups = runner

	issueKit(t, h, s)
	select {
	case run := <-runner.runs:
		t.Fatalf("a backup started before the kit was confirmed: %v", run)
	default:
	}
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)
	select {
	case run := <-runner.runs:
		// The one option records the confirming user.
		if want := []string{backup.RunReplace, "1", "s3", "nfs"}; !reflect.DeepEqual(run, want) {
			t.Errorf("backup %v, want %v", run, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no backup started after the kit was confirmed")
	}
}

func TestBreakGlassIsListedAndNotRevokedAlone(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	issueKit(t, h, s)
	items := accessList(t, h, s)
	if len(items) != 2 || items[0].Name == nil || *items[0].Name != access.BreakGlass || items[0].BreakGlass == nil || !*items[0].BreakGlass {
		t.Fatalf("listed %+v", items)
	}
	talosconfig := items[1]
	notAfter := getAppliance(t).Status.RecoveryKit.TalosconfigNotAfter
	if talosconfig.Kind != Talosconfig || talosconfig.Role != access.RoleOSAdmin || talosconfig.Expires == nil ||
		!talosconfig.Expires.Equal(notAfter.Time) || talosconfig.IssuedBy != testUser {
		t.Errorf("the kit's talosconfig is listed as %+v, want os:admin until %v", talosconfig, notAfter)
	}
	rec := h.request(http.MethodDelete, pathKubeconfig+"/"+access.BreakGlass, nil, s)
	wantStatus(t, rec, http.StatusConflict)
	if len(accessList(t, h, s)) != 2 {
		t.Error("the break-glass kubeconfig was revoked")
	}
}

// failingStatus makes the API's Appliance status updates fail until the test ends.
func failingStatus(t *testing.T, h *harness) {
	t.Helper()
	working := h.srv.deps.Client
	t.Cleanup(func() { h.srv.deps.Client = working })
	h.srv.deps.Client = failingStatusClient{working}
}

type failingStatusClient struct{ client.Client }

func (c failingStatusClient) Status() client.SubResourceWriter {
	return failingStatusWriter{c.Client.Status()}
}

type failingStatusWriter struct{ client.SubResourceWriter }

func (failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("etcd unavailable")
}

// wantKitFollowsSecret checks that the API reports the kit of the stored recipient, unconfirmed, and that the
// console can only replace it.
func wantKitFollowsSecret(t *testing.T, h *harness, s *session) {
	t.Helper()
	recipient := storedRecipient(t)
	if k := kitState(t, h, s); !k.Issued || k.Confirmed || k.Recipient == nil || *k.Recipient != recipient {
		t.Errorf("kit %+v, want the stored recipient %s, unconfirmed", k, recipient)
	}
	statusRec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
	wantStatus(t, statusRec, http.StatusOK)
	var status Status
	decode(t, statusRec, &status)
	if k := status.RecoveryKit; k == nil || !k.Issued || k.Confirmed || k.Recipient == nil || *k.Recipient != recipient {
		t.Errorf("status recovery kit %+v, want the stored recipient, unconfirmed", k)
	}
	rec := h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s)
	wantStatus(t, rec, http.StatusConflict)
	rec = confirmStored(t, h, s)
	wantStatus(t, rec, http.StatusConflict)
	var e Error
	decode(t, rec, &e)
	if e.Code != "kit_not_delivered" {
		t.Errorf("confirm refused with %s, want kit_not_delivered", e.Code)
	}
}

// wantReplaceRepairs replaces the kit and checks that the status mirrors the stored recipient again.
func wantReplaceRepairs(t *testing.T, h *harness, s *session) {
	t.Helper()
	kit := openKit(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(nextPassphrase), Confirm: true}, s), nextPassphrase)
	recipient := kit.BackupKey.Recipient().String()
	if st := getAppliance(t).Status.RecoveryKit; storedRecipient(t) != recipient || st == nil || st.Recipient != recipient {
		t.Errorf("after replace: stored %s, status %+v, kit %s", storedRecipient(t), st, recipient)
	}
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)
}

func TestFailedStatusWriteLeavesReplace(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	issueKit(t, h, s)
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)
	before := storedRecipient(t)

	restore := h.srv.deps.Client
	failingStatus(t, h)
	rec := h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(kitPassphrase), Confirm: true}, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if storedRecipient(t) == before || getAppliance(t).Status.RecoveryKit.Recipient != before {
		t.Fatal("the test needs the Secret to hold the new key and the status the old one")
	}
	h.srv.deps.Client = restore

	wantKitFollowsSecret(t, h, s)
	wantReplaceRepairs(t, h, s)
}

func TestFailedFirstIssueLeavesReplace(t *testing.T) {
	for name, fail := range map[string]func(*testing.T, *harness){
		"status": failingStatus,
		"audit":  func(_ *testing.T, h *harness) { h.srv.deps.Audit = failingAudit{} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			s := h.signIn()
			withKitAccess(t, h)
			working, audit := h.srv.deps.Client, h.srv.deps.Audit
			fail(t, h)
			rec := h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s)
			wantStatus(t, rec, http.StatusInternalServerError)
			if storedRecipient(t) == "" {
				t.Fatal("the test needs the Secret written")
			}
			h.srv.deps.Client, h.srv.deps.Audit = working, audit

			wantKitFollowsSecret(t, h, s)
			wantReplaceRepairs(t, h, s)
		})
	}
}

func TestReissueKeepsBackupKey(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	first := issueKit(t, h, s)
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)
	recipient := storedRecipient(t)

	for _, tc := range []struct {
		name   string
		parts  map[string][]byte
		code   string
		status int
	}{
		{"wrong passphrase", map[string][]byte{"kit": first, "passphrase": []byte("not the passphrase"), "newPassphrase": []byte(nextPassphrase)}, "wrong_passphrase", http.StatusBadRequest},
		{"short new passphrase", map[string][]byte{"kit": first, "passphrase": []byte(kitPassphrase), "newPassphrase": []byte("short")}, "invalid", http.StatusBadRequest},
		{"no kit", map[string][]byte{"passphrase": []byte(kitPassphrase), "newPassphrase": []byte(nextPassphrase)}, "invalid", http.StatusBadRequest},
		{"not a kit", map[string][]byte{"kit": []byte("not a kit"), "passphrase": []byte(kitPassphrase), "newPassphrase": []byte(nextPassphrase)}, "invalid_kit", http.StatusBadRequest},
	} {
		rec := h.upload(http.MethodPost, pathReissue, tc.parts, s)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d: %s", tc.name, rec.Code, rec.Body)
			continue
		}
		var e Error
		decode(t, rec, &e)
		if e.Code != tc.code {
			t.Errorf("%s: code %s", tc.name, e.Code)
		}
	}

	kit := openKit(t, reissue(h, s, first, kitPassphrase, nextPassphrase), nextPassphrase)
	if kit.BackupKey.Recipient().String() != recipient || storedRecipient(t) != recipient {
		t.Error("reissue changed the backup key")
	}
	if st := getAppliance(t).Status.RecoveryKit; st == nil || st.Confirmed || st.Recipient != recipient {
		t.Errorf("status after reissue %+v", st)
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRecoveryKitReissue) || e.Result != string(Success) {
		t.Errorf("audit entry %+v", e)
	}
}

func TestReissueRejectsForeignKit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := recovery.Build(recovery.Kit{BackupKey: key, Kubeconfig: []byte("x"), Talosconfig: []byte("y")}, kitPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	rec := reissue(h, s, foreign, kitPassphrase, nextPassphrase)
	wantStatus(t, rec, http.StatusConflict)

	issueKit(t, h, s)
	uids, recipient, status := breakGlassUIDs(t), storedRecipient(t), getAppliance(t).Status.RecoveryKit
	rec = reissue(h, s, foreign, kitPassphrase, nextPassphrase)
	wantStatus(t, rec, http.StatusConflict)
	var e Error
	decode(t, rec, &e)
	if e.Code != "foreign_kit" {
		t.Errorf("code %s", e.Code)
	}
	if breakGlassUIDs(t) != uids {
		t.Error("a refused reissue replaced the break-glass credentials")
	}
	if storedRecipient(t) != recipient || !reflect.DeepEqual(getAppliance(t).Status.RecoveryKit, status) {
		t.Error("a refused reissue changed the recipient or the status")
	}
}

// TestKitRefusedWhileARestoreIsPending: a kit issued before the restore would be overwritten by the backup's recipient.
func TestKitRefusedWhileARestoreIsPending(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	a := getAppliance(t)
	a.Status.Setup = v1alpha1.SetupRestore
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"issue":   h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s),
		"replace": h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(kitPassphrase), Confirm: true}, s),
	} {
		wantStatus(t, rec, http.StatusConflict)
		var e Error
		decode(t, rec, &e)
		if e.Code != codeRestorePending {
			t.Errorf("%s: code %q, want %s", name, e.Code, codeRestorePending)
		}
	}
	if !secretGone(t, recovery.RecipientSecret) {
		t.Error("a kit was issued during a restore")
	}
}

func TestConcurrentFirstIssuesStoreOneKey(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	var wg sync.WaitGroup
	answers := make([]*httptest.ResponseRecorder, 2)
	for i := range answers {
		wg.Go(func() {
			answers[i] = h.request(http.MethodPost, pathRecoveryKit, RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}, s)
		})
	}
	wg.Wait()
	var issued []*httptest.ResponseRecorder
	for _, rec := range answers {
		switch rec.Code {
		case http.StatusOK:
			issued = append(issued, rec)
		case http.StatusConflict:
		default:
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
	if len(issued) != 1 {
		t.Fatalf("%d kits issued, want 1", len(issued))
	}
	recipient := openKit(t, issued[0], kitPassphrase).BackupKey.Recipient().String()
	if storedRecipient(t) != recipient || getAppliance(t).Status.RecoveryKit.Recipient != recipient {
		t.Error("the stored recipient is not the issued kit's")
	}
}

func TestReplaceNewKey(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	wantStatus(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(kitPassphrase), Confirm: true}, s), http.StatusConflict)
	first := recoveryKey(t, issueKit(t, h, s), kitPassphrase)
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)

	rec := h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(nextPassphrase)}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantStatus(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr("short"), Confirm: true}, s), http.StatusBadRequest)

	kit := openKit(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(nextPassphrase), Confirm: true}, s), nextPassphrase)
	recipient := kit.BackupKey.Recipient().String()
	if recipient == first || storedRecipient(t) != recipient {
		t.Error("replace kept the backup key")
	}
	if st := getAppliance(t).Status.RecoveryKit; st == nil || st.Confirmed || st.Recipient != recipient {
		t.Errorf("status after replace %+v", st)
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRecoveryKitReplace) || e.Details["recipient"] != recipient {
		t.Errorf("audit entry %+v", e)
	}
}

func recoveryKey(t *testing.T, data []byte, passphrase string) string {
	t.Helper()
	kit, err := recovery.Open(data, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	return kit.BackupKey.Recipient().String()
}

func TestBreakGlassRevokedOnReissue(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	first := issueKit(t, h, s)
	before := breakGlassUIDs(t)
	wantStatus(t, reissue(h, s, first, kitPassphrase, nextPassphrase), http.StatusOK)
	afterReissue := breakGlassUIDs(t)
	wantStatus(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(kitPassphrase), Confirm: true}, s), http.StatusOK)
	afterReplace := breakGlassUIDs(t)
	for i := range before {
		if before[i] == afterReissue[i] || afterReissue[i] == afterReplace[i] {
			t.Errorf("the break-glass objects were not recreated: %v %v %v", before, afterReissue, afterReplace)
		}
	}
}

// breakGlassUIDs returns the UIDs of the break-glass ServiceAccount and its token Secret.
func breakGlassUIDs(t *testing.T) [2]string {
	t.Helper()
	ctx := context.Background()
	sa, secrets := &corev1.ServiceAccount{}, &corev1.SecretList{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: access.Namespace, Name: access.BreakGlass}, sa); err != nil {
		t.Fatal(err)
	}
	if err := testClient.List(ctx, secrets, client.InNamespace(access.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets.Items {
		if len(s.OwnerReferences) == 1 && s.OwnerReferences[0].UID == sa.UID {
			return [2]string{string(sa.UID), string(s.UID)}
		}
	}
	t.Fatal("the break-glass ServiceAccount owns no token Secret")
	return [2]string{}
}

func TestPrivateKeyNeverStored(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withKitAccess(t, h)
	first := issueKit(t, h, s)
	wantStatus(t, reissue(h, s, first, kitPassphrase, nextPassphrase), http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(kitPassphrase), Confirm: true}, s), http.StatusOK)

	ctx := context.Background()
	var stored strings.Builder
	for _, list := range []client.ObjectList{&corev1.SecretList{}, &corev1.ConfigMapList{}, &v1alpha1.ApplianceList{}} {
		if err := testClient.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		stored.Write(data)
	}
	if !strings.Contains(stored.String(), storedRecipient(t)) {
		t.Fatal("the stored objects do not hold the recipient; the test would not notice the key")
	}
	if strings.Contains(stored.String(), "AGE-SECRET-KEY-") {
		t.Error("a Secret, ConfigMap or the Appliance holds a backup private key")
	}
}

func TestKitNeedsTheNode(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	body := RecoveryKitRequest{Passphrase: ptr(kitPassphrase)}
	wantStatus(t, h.request(http.MethodPost, pathRecoveryKit, body, s), http.StatusConflict)
	wantStatus(t, confirmStored(t, h, s), http.StatusConflict)
	withAccess(t)
	fillAccessTokens(t)
	wantStatus(t, h.request(http.MethodPost, pathRecoveryKit, body, s), http.StatusServiceUnavailable)
	h.withTalos(&talosNode{err: errors.New("rpc error: code = Unavailable")}, nil)
	wantStatus(t, h.request(http.MethodPost, pathRecoveryKit, body, s), http.StatusServiceUnavailable)
	wantStatus(t, confirmStored(t, h, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodPost, pathRecoveryKit, nil, s), http.StatusBadRequest)
	wantStatus(t, h.request(http.MethodPost, pathReplace, nil, s), http.StatusBadRequest)
	if items := accessList(t, h, s); len(items) != 0 {
		t.Errorf("break-glass issued without a kit: %+v", items)
	}
	if _, err := recovery.StoredRecipient(context.Background(), testClient); err == nil {
		t.Error("a recipient was stored without a kit")
	}
}
