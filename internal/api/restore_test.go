package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
)

const (
	pathRestore        = "/appliance/api/v1/restore"
	pathRestoreUpload  = pathRestore + "/upload"
	pathRestoreList    = pathRestore + "/list"
	pathRestoreFetch   = pathRestore + "/fetch"
	pathRestoreInspect = pathRestore + "/inspect"
	pathRestoreStart   = pathRestore + "/start"
	pathRestoreResume  = pathRestore + "/resume"

	restorePassphrase = "restore-Passphrase-that-must-stay-inside"
	restoreS3Secret   = "restore-s3-secret-key-never-leaves"
	restoreSFTPPass   = "restore-sftp-Pa55word-never-leaves"
	restoreName       = "old-node-20261101T020000Z-0.1.0.ilmbackup"
	restoreILMHost    = "ilm.old.example.com"
	restoreNodeHost   = "old-node"
)

// fakeRestorer records the restores it is asked to start and resume.
type fakeRestorer struct {
	started   []restoreCall
	startErr  error
	resumed   []age.Identity
	resumeErr error
}

type restoreCall struct {
	file string
	id   age.Identity
	opts restore.Options
}

func (f *fakeRestorer) Start(_ context.Context, file string, id age.Identity, opts restore.Options) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, restoreCall{file: file, id: id, opts: opts})
	return nil
}

func (f *fakeRestorer) Resume(_ context.Context, id age.Identity) error {
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.resumed = append(f.resumed, id)
	return nil
}

// withRestore gives the harness's server a fakeRestorer and a restore directory.
func withRestore(t *testing.T, h *harness) (*fakeRestorer, string) {
	t.Helper()
	f := &fakeRestorer{}
	h.srv.deps.Restore = f
	h.srv.deps.RestoreDir = filepath.Join(t.TempDir(), "restore")
	return f, h.srv.deps.RestoreDir
}

// restoreKit is a recovery kit encrypted with restorePassphrase and its backup key.
func restoreKit(t *testing.T) (*age.X25519Identity, []byte) {
	t.Helper()
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	kit, err := recovery.Build(recovery.Kit{BackupKey: key, Kubeconfig: []byte("k"), Talosconfig: []byte("t")}, restorePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	return key, kit
}

// usersYAML is the Secret appliance-users holding the named accounts, as a backup stores it.
func usersYAML(t *testing.T, names ...string) string {
	t.Helper()
	c := fake.NewClientBuilder().Build()
	store := auth.NewSecretUserStore(c, SystemNamespace)
	for _, n := range names {
		if err := store.Create(context.Background(), auth.User{Name: n, PasswordHash: "hash", TOTPSeed: "SEED"}); err != nil {
			t.Fatal(err)
		}
	}
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: SystemNamespace, Name: auth.UsersSecret}, secret); err != nil {
		t.Fatal(err)
	}
	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	out, err := yaml.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// sealedBackup is a backup of ILM version ilm with the named console accounts, encrypted to key.
func sealedBackup(t *testing.T, key *age.X25519Identity, ilm string, accounts ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := backup.NewWriter(&buf, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]string{"appliance.yaml": "kind: Appliance\n"}
	if len(accounts) > 0 {
		members["secrets/appliance-system/appliance-users.yaml"] = usersYAML(t, accounts...)
	}
	for _, name := range []string{"appliance.yaml", "secrets/appliance-system/appliance-users.yaml"} {
		if body, ok := members[name]; ok {
			if err := w.Add(name, int64(len(body)), strings.NewReader(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	m := backup.Manifest{
		Appliance: "0.1.0", ILM: ilm, Operator: "1.1.0", Components: []string{"core"}, Database: "managed",
		DatabaseIncluded: true, HostName: restoreNodeHost, ILMHostName: restoreILMHost, Time: time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC),
	}
	if err := w.Close(m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// putRestoreFile writes data as the uploaded backup name.
func putRestoreFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// uploadBackup sends body as the backup name with the Content-Length size.
func (h *harness) uploadBackup(name string, body io.Reader, size int64, s *session) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPut, pathRestoreUpload+"?name="+name, body)
	req.ContentLength = size
	req.RemoteAddr = testPeer
	req.Header.Set("Content-Type", "application/octet-stream")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.cookie})
	req.Header.Set(auth.CSRFHeader, s.csrf)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) inspect(source, name string, kit []byte, passphrase string, s *session) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.upload(http.MethodPost, pathRestoreInspect, map[string][]byte{
		"source": []byte(source), "name": []byte(name), "kit": kit, "passphrase": []byte(passphrase),
	}, s)
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	wantStatus(t, rec, status)
	var e Error
	decode(t, rec, &e)
	if e.Code != code {
		t.Errorf("code %q, want %q: %s", e.Code, code, rec.Body)
	}
}

func setStatus(t *testing.T, change func(*v1alpha1.ApplianceStatus)) {
	t.Helper()
	a := getAppliance(t)
	change(&a.Status)
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// freshRestore is a signed-in harness on an appliance that waits for a restore, with an uploaded backup of the
// release's ILM version holding the accounts admin and alice.
func freshRestore(t *testing.T) (*harness, *session, *fakeRestorer, *age.X25519Identity, []byte) {
	t.Helper()
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupRestore)
	f, dir := withRestore(t, h)
	key, kit := restoreKit(t)
	putRestoreFile(t, dir, restoreName, sealedBackup(t, key, testRelease.Platform.ILMVersion, testUser, "alice"))
	return h, s, f, key, kit
}

func TestRestoreStartNeedsInspect(t *testing.T) {
	h, s, f, key, kit := freshRestore(t)
	start := RestoreStart{Confirm: true, IlmHostName: ptr("ilm.new.example.com")}

	wantCode(t, h.request(http.MethodPost, pathRestoreStart, start, s), http.StatusConflict, "not_inspected")
	wantStatus(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusOK)
	other := h.signIn()
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, start, other), http.StatusConflict, "not_inspected")
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{}, s), http.StatusBadRequest, codeInvalid)
	if len(f.started) != 0 {
		t.Fatalf("restore started without its inspect: %+v", f.started)
	}

	wantStatus(t, h.request(http.MethodPost, pathRestoreStart, start, s), http.StatusAccepted)
	if len(f.started) != 1 {
		t.Fatalf("started %d restores, want 1", len(f.started))
	}
	got := f.started[0]
	if got.file != filepath.Join(h.srv.deps.RestoreDir, restoreName) || got.opts != (restore.Options{ILMHostName: "ilm.new.example.com", User: testUser}) {
		t.Errorf("started %s with %+v", got.file, got.opts)
	}
	if id, ok := got.id.(*age.X25519Identity); !ok || id.String() != key.String() {
		t.Error("the restore did not get the kit's backup key")
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRestoreStart) || e.Details["backup"] != restoreName || e.Result != string(Success) {
		t.Errorf("audit entry %+v", e)
	}
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, start, s), http.StatusConflict, "not_inspected")
	if e := lastEntry(h); e.Result != string(Failure) || e.Details["refused"] != "not_inspected" {
		t.Errorf("audit entry %+v", e)
	}
}

func TestInPlaceNeedsSameVersion(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	f, _ := withRestore(t, h)
	_, local := withRunner(t, h)
	key, kit := restoreKit(t)
	running := testRelease.Platform.ILMVersion
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Versions.ILM = running })
	same := backup.FileName(backupHost, time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC), backupTestVer)
	older := backup.FileName(backupHost, time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC), backupTestVer)
	putRestoreFile(t, local, same, sealedBackup(t, key, running))
	putRestoreFile(t, local, older, sealedBackup(t, key, "2.19.0"))

	rec := h.inspect("local", older, kit, restorePassphrase, s)
	wantCode(t, rec, http.StatusConflict, "version_unavailable")
	if !strings.Contains(rec.Body.String(), "2.19.0") || !strings.Contains(rec.Body.String(), running) {
		t.Errorf("the refusal does not name both versions: %s", rec.Body)
	}

	wantStatus(t, h.inspect("local", same, kit, restorePassphrase, s), http.StatusOK)
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true}, s), http.StatusConflict, "not_local")
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Versions.ILM = "2.21.0" })
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true, InPlace: true}, s), http.StatusConflict, "version_unavailable")
	if len(f.started) != 0 {
		t.Fatalf("restored in place over another ILM version: %+v", f.started)
	}
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Versions.ILM = running })
	wantStatus(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true, InPlace: true}, s), http.StatusAccepted)
	if len(f.started) != 1 || !f.started[0].opts.InPlace || f.started[0].file != filepath.Join(local, same) {
		t.Errorf("started %+v", f.started)
	}

	// A local backup is restored only in place, and an uploaded one only onto a fresh appliance.
	wantCode(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusConflict, "setup")
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Setup = v1alpha1.SetupRestore })
	wantCode(t, h.inspect("local", same, kit, restorePassphrase, s), http.StatusConflict, "setup")
}

func TestUploadRefusesLowSpace(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupRestore)
	_, dir := withRestore(t, h)
	var checked []uint64
	h.srv.checkSpace = func(path string, expected uint64) error {
		if path != dir {
			t.Errorf("checked the space of %s, want %s", path, dir)
		}
		checked = append(checked, expected)
		return fmt.Errorf("%w: test", storage.ErrNoSpace)
	}
	body := &countingReader{r: bytes.NewReader(make([]byte, 4096))}
	wantCode(t, h.uploadBackup(restoreName, body, 4096, s), http.StatusConflict, "no_space")
	if !slices.Equal(checked, []uint64{4096}) || body.n != 0 {
		t.Errorf("checked %v, read %d bytes before the check", checked, body.n)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused upload left %v", entries)
	}
}

// countingReader counts what is read from r.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestUploadStoresTheBackup(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupRestore)
	_, dir := withRestore(t, h)
	putRestoreFile(t, dir, "earlier.ilmbackup", []byte("old"))
	data := bytes.Repeat([]byte("b"), 10000)

	wantStatus(t, h.uploadBackup(restoreName, bytes.NewReader(data), int64(len(data)), s), http.StatusNoContent)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != restoreName {
		t.Fatalf("restore directory %v: %v", entries, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, restoreName)); !bytes.Equal(got, data) {
		t.Error("the stored backup differs from the upload")
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRestoreUpload) || e.Details["backup"] != restoreName || e.Result != string(Success) {
		t.Errorf("audit entry %+v", e)
	}

	for _, name := range []string{"x.txt", ".ilmbackup", "x.ilmbackup.part", ""} {
		rec := h.uploadBackup(name, bytes.NewReader(data), int64(len(data)), s)
		wantStatus(t, rec, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "ending in .ilmbackup") {
			t.Errorf("%q: the refusal does not name the rule: %s", name, rec.Body)
		}
	}
	wantCode(t, h.uploadBackup(restoreName, bytes.NewReader(data), -1, s), http.StatusBadRequest, codeInvalid)
	wantCode(t, h.uploadBackup("short.ilmbackup", bytes.NewReader(data), int64(len(data))+5, s), http.StatusBadRequest, "incomplete")
	if _, err := os.Stat(filepath.Join(dir, "short.ilmbackup")); !os.IsNotExist(err) {
		t.Errorf("an incomplete upload was kept: %v", err)
	}

	setStatus(t, func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{Backup: restoreName, Step: v1alpha1.RestoreStepDatabase}
	})
	wantCode(t, h.uploadBackup(restoreName, bytes.NewReader(data), int64(len(data)), s), http.StatusConflict, "busy")
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Restore, st.Setup = nil, v1alpha1.SetupComplete })
	wantCode(t, h.uploadBackup(restoreName, bytes.NewReader(data), int64(len(data)), s), http.StatusConflict, "setup")
}

// TestUploadNormalisesTheName stores a backup under a safe name, as browsers name a second download.
func TestUploadNormalisesTheName(t *testing.T) {
	h, s, _, _, kit := freshRestore(t)
	dir := h.srv.deps.RestoreDir
	data, err := os.ReadFile(filepath.Join(dir, restoreName))
	if err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string]string{
		"x%20(1).ilmbackup":       "x__1_.ilmbackup",
		"..%2Fa.ilmbackup":        "_._a.ilmbackup",
		"%C3%A9t%C3%A9.ilmbackup": "_t_.ilmbackup",
	} {
		wantStatus(t, h.uploadBackup(name, bytes.NewReader(data), int64(len(data)), s), http.StatusNoContent)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || entries[0].Name() != stored {
			t.Errorf("%s: restore directory %v (%v), want %s", name, entries, err, stored)
		}
	}
	wantStatus(t, h.uploadBackup("x%20(1).ilmbackup", bytes.NewReader(data), int64(len(data)), s), http.StatusNoContent)
	wantStatus(t, h.inspect("upload", "x (1).ilmbackup", kit, restorePassphrase, s), http.StatusOK)
}

// TestUploadKeepsTheFailedRestoresBackup keeps the backup a failed restore resumes from when another one arrives.
func TestUploadKeepsTheFailedRestoresBackup(t *testing.T) {
	h, s, _, _, _ := freshRestore(t)
	dir := h.srv.deps.RestoreDir
	putRestoreFile(t, dir, "earlier.ilmbackup", []byte("old"))
	failed := filepath.Join(dir, restoreName)
	setStatus(t, func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{Backup: restoreName, File: failed, Step: v1alpha1.RestoreStepDatabase, Message: v1alpha1.RestoreFailed}
	})
	data := bytes.Repeat([]byte("b"), 100)
	wantStatus(t, h.uploadBackup("other.ilmbackup", bytes.NewReader(data), int64(len(data)), s), http.StatusNoContent)
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, slices.Sorted(slices.Values([]string{"other.ilmbackup", restoreName}))) {
		t.Errorf("restore directory %v, want the failed restore's backup and the new one", names)
	}
}

func TestRestoreStartWhileTheBackupChanges(t *testing.T) {
	h, s, f, _, kit := freshRestore(t)
	wantStatus(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusOK)
	h.srv.restoreMu.Lock()
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true}, s), http.StatusConflict, "busy")
	h.srv.restoreMu.Unlock()
	if len(f.started) != 0 {
		t.Errorf("started %+v while an upload held the backup", f.started)
	}
	wantStatus(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true}, s), http.StatusAccepted)
}

func TestInspectRefusals(t *testing.T) {
	h, s, _, _, kit := freshRestore(t)
	dir := h.srv.deps.RestoreDir
	_, foreign := restoreKit(t)

	rec := h.inspect("upload", restoreName, kit, "not the passphrase at all", s)
	wantCode(t, rec, http.StatusBadRequest, "wrong_passphrase")
	wantCode(t, h.inspect("upload", restoreName, []byte("garbage"), restorePassphrase, s), http.StatusBadRequest, "invalid_kit")
	wantCode(t, h.inspect("upload", restoreName, foreign, restorePassphrase, s), http.StatusConflict, "foreign_kit")
	wantCode(t, h.inspect("upload", "missing.ilmbackup", kit, restorePassphrase, s), http.StatusNotFound, codeNotFound)
	wantCode(t, h.inspect("upload", "backup.tar", kit, restorePassphrase, s), http.StatusBadRequest, codeInvalid)
	wantCode(t, h.inspect("elsewhere", restoreName, kit, restorePassphrase, s), http.StatusBadRequest, codeInvalid)

	data, err := os.ReadFile(filepath.Join(dir, restoreName))
	if err != nil {
		t.Fatal(err)
	}
	changed := slices.Clone(data)
	changed[len(changed)/2] ^= 0xff
	putRestoreFile(t, dir, "cut.ilmbackup", data[:len(data)/2])
	putRestoreFile(t, dir, "changed.ilmbackup", changed)
	for _, name := range []string{"cut.ilmbackup", "changed.ilmbackup"} {
		rec = h.inspect("upload", name, kit, restorePassphrase, s)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "tampered") && !strings.Contains(rec.Body.String(), "truncated") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	e := lastEntry(h)
	if e.Action != string(AuditEntryActionRestoreInspect) || e.Result != string(Failure) || e.Details["backup"] != "changed.ilmbackup" {
		t.Errorf("audit entry %+v", e)
	}
}

func TestInspectSummary(t *testing.T) {
	h, s, _, _, kit := freshRestore(t)
	rec := h.inspect("upload", restoreName, kit, restorePassphrase, s)
	wantStatus(t, rec, http.StatusOK)
	var got RestoreSummary
	decode(t, rec, &got)
	if got.Backup != restoreName || got.HostName != restoreNodeHost || got.NodeHostName != "appliance.example.com" ||
		got.IlmHostName != restoreILMHost || got.Ilm != testRelease.Platform.ILMVersion || got.InPlace ||
		!slices.Equal(got.Accounts, []string{testUser, "alice"}) || !slices.Equal(got.Clashes, []string{testUser}) ||
		got.Database != "managed" || !got.DatabaseIncluded || !slices.Equal(got.Components, []string{"core"}) {
		t.Errorf("summary %+v", got)
	}
	e := lastEntry(h)
	if e.Action != string(AuditEntryActionRestoreInspect) || e.Result != string(Success) || e.Details["backup"] != restoreName {
		t.Errorf("audit entry %+v", e)
	}
}

func TestRestoreStartRefusals(t *testing.T) {
	h, s, f, _, kit := freshRestore(t)
	wantStatus(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusOK)
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true, IlmHostName: ptr("not a host")}, s),
		http.StatusBadRequest, codeInvalid)
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true, InPlace: true}, s), http.StatusConflict, "not_local")
	for err, code := range map[error]string{
		restore.ErrBusy:              "busy",
		restore.ErrSetup:             "setup",
		restore.ErrUpgradeInProgress: "upgrade_in_progress",
		restore.ErrBackupRunning:     "backup_running",
	} {
		f.startErr = err
		wantCode(t, h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true}, s), http.StatusConflict, code)
	}
	f.startErr = errors.New("open /data/restore/secret-path: denied")
	rec := h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true}, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String()+h.logs.String(), "secret-path") {
		t.Error("the engine's error text left the manager")
	}
}

func TestRestoreResume(t *testing.T) {
	h, s, f, key, kit := freshRestore(t)
	resume := func(kit []byte, passphrase string) *httptest.ResponseRecorder {
		return h.upload(http.MethodPost, pathRestoreResume, map[string][]byte{"kit": kit, "passphrase": []byte(passphrase)}, s)
	}
	wantCode(t, resume(kit, "not the passphrase at all"), http.StatusBadRequest, "wrong_passphrase")
	wantCode(t, resume([]byte("garbage"), restorePassphrase), http.StatusBadRequest, "invalid_kit")
	for err, code := range map[error]string{
		restore.ErrNotWaiting:   "not_waiting",
		restore.ErrBusy:         "busy",
		restore.ErrForeignKit:   "foreign_kit",
		restore.ErrTampered:     "tampered",
		restore.ErrTruncated:    "truncated",
		restore.ErrFormatTooNew: "format_too_new",
	} {
		f.resumeErr = err
		wantCode(t, resume(kit, restorePassphrase), http.StatusConflict, code)
	}
	f.resumeErr = nil
	setStatus(t, func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{Backup: restoreName, Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepDatabase}
	})
	wantStatus(t, resume(kit, restorePassphrase), http.StatusAccepted)
	if len(f.resumed) != 1 || f.resumed[0].(*age.X25519Identity).String() != key.String() {
		t.Errorf("resumed %d times without the kit's key", len(f.resumed))
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRestoreResume) || e.Details["backup"] != restoreName || e.Result != string(Success) {
		t.Errorf("audit entry %+v", e)
	}
}

func TestGetRestore(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodGet, pathRestore, nil, s), http.StatusConflict)
	createAppliance(t, v1alpha1.SetupRestore)
	rec := h.request(http.MethodGet, pathRestore, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got RestoreState
	decode(t, rec, &got)
	if got.Setup != v1alpha1.SetupRestore || got.Restore != nil {
		t.Errorf("state %+v", got)
	}
	started := metav1.NewTime(time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC))
	setStatus(t, func(st *v1alpha1.ApplianceStatus) {
		st.Restore = &v1alpha1.RestoreStatus{Backup: restoreName, File: "/data/restore/" + restoreName, ILMHostName: "ilm.new.example.com",
			Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepDatabase, StartedAt: started}
	})
	rec = h.request(http.MethodGet, pathRestore, nil, s)
	decode(t, rec, &got)
	p := got.Restore
	if p == nil || p.Backup != restoreName || p.Step != "waitingForKit" || deref(p.PausedStep) != "database" ||
		deref(p.IlmHostName) != "ilm.new.example.com" || p.StartedAt == nil || !p.StartedAt.Equal(started.Time) || p.Message != nil {
		t.Errorf("progress %+v", p)
	}
	if strings.Contains(rec.Body.String(), "/data/restore") {
		t.Errorf("the answer names the file's path: %s", rec.Body)
	}
}

// memoryTarget is a target of backup files in memory whose operations fail with err.
type memoryTarget struct {
	objects  map[string][]byte
	modified map[string]time.Time
	err      error
}

func (m *memoryTarget) Put(context.Context, string, io.Reader, int64) error { return m.err }
func (m *memoryTarget) Delete(context.Context, string) error                { return m.err }

func (m *memoryTarget) List(context.Context) ([]target.Object, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []target.Object
	for name, data := range m.objects {
		out = append(out, target.Object{Name: name, Size: int64(len(data)), Modified: m.modified[name]})
	}
	return out, nil
}

func (m *memoryTarget) Get(_ context.Context, name string) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	return io.NopCloser(bytes.NewReader(m.objects[name])), nil
}

// enteringRunner is a fakeRunner that hands out its target for any target entered for a restore.
type enteringRunner struct {
	*fakeRunner
	target target.Target
	specs  []v1alpha1.BackupTarget
	creds  []map[string][]byte
}

func (r *enteringRunner) EnteredTarget(_ context.Context, spec v1alpha1.BackupTarget, creds map[string][]byte) (target.Target, error) {
	r.specs, r.creds = append(r.specs, spec), append(r.creds, creds)
	return r.target, nil
}

func withTarget(t *testing.T, h *harness, tgt target.Target) *enteringRunner {
	t.Helper()
	f, _ := newFakeRunner(t)
	r := &enteringRunner{fakeRunner: f, target: tgt}
	h.srv.deps.Backups = r
	return r
}

func restoreS3Target() BackupTargetUpdate {
	return s3Update(ptr(s3AccessKey), ptr(restoreS3Secret))
}

func TestRestoreFromATarget(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupRestore)
	_, dir := withRestore(t, h)
	newer, data := "node-20261102T020000Z-0.1.0.ilmbackup", []byte("the newer backup")
	mem := &memoryTarget{
		objects:  map[string][]byte{restoreName: []byte("older"), newer: data},
		modified: map[string]time.Time{restoreName: time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC), newer: time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)},
	}
	r := withTarget(t, h, mem)

	rec := h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: restoreS3Target()}, s)
	wantStatus(t, rec, http.StatusOK)
	var files RestoreFiles
	decode(t, rec, &files)
	if len(files.Files) != 2 || files.Files[0].Name != newer || files.Files[0].SizeBytes != int64(len(data)) {
		t.Errorf("files %+v", files)
	}
	if spec := r.specs[0]; spec.Type != v1alpha1.BackupTypeS3 || spec.S3.Bucket != "ilm" ||
		string(r.creds[0][target.SecretKeyKey]) != restoreS3Secret || string(r.creds[0][target.AccessKeyKey]) != s3AccessKey {
		t.Errorf("target %+v with %v", spec, r.creds[0])
	}

	wantStatus(t, h.request(http.MethodPost, pathRestoreFetch, RestoreFetch{Target: restoreS3Target(), Name: newer}, s), http.StatusNoContent)
	if got, _ := os.ReadFile(filepath.Join(dir, newer)); !bytes.Equal(got, data) {
		t.Errorf("fetched %q", got)
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionRestoreFetch) || e.Details["backup"] != newer || e.Details["type"] != "s3" {
		t.Errorf("audit entry %+v", e)
	}
	wantCode(t, h.request(http.MethodPost, pathRestoreFetch, RestoreFetch{Target: restoreS3Target(), Name: "gone.ilmbackup"}, s), http.StatusNotFound, codeNotFound)
	wantCode(t, h.request(http.MethodPost, pathRestoreFetch, RestoreFetch{Target: restoreS3Target(), Name: "../x"}, s), http.StatusBadRequest, codeInvalid)

	noSecret := s3Update(ptr(s3AccessKey), nil)
	rec = h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: noSecret}, s)
	wantTargetField(t, rec, "s3.secretKey")

	mem.err = fmt.Errorf("%w: %s %s", target.ErrAuthFailed, targetLeak, restoreS3Secret)
	wantCode(t, h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: restoreS3Target()}, s), http.StatusBadGateway, target.ReasonAuthFailed)
	wantCode(t, h.request(http.MethodPost, pathRestoreFetch, RestoreFetch{Target: restoreS3Target(), Name: newer}, s), http.StatusBadGateway, target.ReasonAuthFailed)
	if strings.Contains(h.logs.String(), targetLeak) {
		t.Error("the log holds the target's error text")
	}

	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Setup = v1alpha1.SetupComplete })
	wantCode(t, h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: restoreS3Target()}, s), http.StatusConflict, "setup")
	h.srv.deps.Backups = nil
	wantStatus(t, h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: restoreS3Target()}, s), http.StatusServiceUnavailable)
}

// restoreSecretAnswers lists and fetches from a target entered for a restore whose driver error repeats the
// password, and inspects and resumes with the kit, adding the answers to answers. It returns the kit's backup key.
func restoreSecretAnswers(t *testing.T, h *harness, s *session, answers *bytes.Buffer) string {
	t.Helper()
	before := getAppliance(t).Status.Setup
	setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Setup = v1alpha1.SetupRestore })
	defer setStatus(t, func(st *v1alpha1.ApplianceStatus) { st.Setup = before })
	f, dir := withRestore(t, h)
	key, kit := restoreKit(t)
	putRestoreFile(t, dir, restoreName, sealedBackup(t, key, testRelease.Platform.ILMVersion, testUser))
	mem := &memoryTarget{err: fmt.Errorf("%w: %s", target.ErrAuthFailed, restoreSFTPPass)}
	runner := h.srv.deps.Backups
	defer func() { h.srv.deps.Backups = runner }()
	withTarget(t, h, mem)
	sftp := sftpUpdate(ptr(restoreSFTPPass), nil)
	for _, rec := range []*httptest.ResponseRecorder{
		h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: sftp}, s),
		h.request(http.MethodPost, pathRestoreFetch, RestoreFetch{Target: sftp, Name: restoreName}, s),
		h.request(http.MethodPost, pathRestoreList, RestoreTargetRequest{Target: restoreS3Target()}, s),
		h.inspect("upload", restoreName, kit, restorePassphrase+"-wrong", s),
		h.inspect("upload", restoreName, kit, restorePassphrase, s),
		h.request(http.MethodPost, pathRestoreStart, RestoreStart{Confirm: true}, s),
		h.upload(http.MethodPost, pathRestoreResume, map[string][]byte{"kit": kit, "passphrase": []byte(restorePassphrase)}, s),
		h.request(http.MethodGet, pathRestore, nil, s),
	} {
		answers.Write(rec.Body.Bytes())
	}
	if len(f.started) != 1 {
		t.Fatalf("started %d restores, want 1", len(f.started))
	}
	return key.String()
}
