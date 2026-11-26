package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle/bundletest"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const (
	pathUpdates      = "/appliance/api/v1/updates"
	pathUpdatesCheck = pathUpdates + "/check"
	pathUpdatesFetch = pathUpdates + "/fetch"
	// zotPassword is the push password of the appliance's registry; registryPullPassword the credentials of the
	// online repository.
	zotPassword          = "zot-push-Pa55word-that-stays-inside"
	registryPullPassword = "ghcr-pull-Pa55word-that-stays-inside"
)

// updatesFixture is the loader and the finder of the harness's server, on directories of the test.
type updatesFixture struct {
	loader  *update.Loader
	runner  *backup.Runner
	key     *ecdsa.PrivateKey
	dir     string
	uploads string
	// noSpace makes the free space check refuse.
	noSpace bool
}

// withUpdates gives the server a loader and a finder whose registry and repository fail with errors that repeat
// their passwords.
func (h *harness) withUpdates() *updatesFixture {
	t := h.t
	t.Helper()
	root := t.TempDir()
	f := &updatesFixture{
		runner: backup.NewRunner(backup.RunnerDeps{}), key: bundletest.NewKey(t),
		dir: filepath.Join(root, "bundles"), uploads: filepath.Join(root, "updates"),
	}
	online := update.Online{
		Repository: "example.com/appliance-ng",
		Options: func(context.Context) ([]remote.Option, error) {
			return nil, errors.New("401 Unauthorized for puller:" + registryPullPassword)
		},
	}
	f.loader = update.NewLoader(update.LoaderDeps{
		Client: testClient,
		Registry: func(context.Context) (update.Registry, error) {
			return nil, errors.New("401 Unauthorized for appliance-manager:" + zotPassword)
		},
		Admission: f.runner,
		Audit:     h.audit,
		Log:       h.srv.deps.Log,
		Release:   testRelease,
		Dir:       f.dir,
		Uploads:   f.uploads,
		Online:    online,
		Key:       &f.key.PublicKey,
		Arch:      "amd64",
		Space:     f.space,
	})
	h.srv.checkSpace = f.space
	h.srv.deps.Updates = f.loader
	h.srv.deps.Finder = update.NewFinder(update.FinderDeps{
		Client: testClient, Online: online, Release: testRelease, Dir: f.dir, Uploads: f.uploads,
		Log: h.srv.deps.Log, Key: &f.key.PublicKey, Arch: "amd64",
	})
	h.srv.deps.UpdatesDir = f.uploads
	return f
}

func (f *updatesFixture) space(string, uint64) error {
	if f.noSpace {
		return storage.ErrNoSpace
	}
	return nil
}

// signedBundle is a signed platform bundle file without images, which every appliance can install.
func (f *updatesFixture) signedBundle(t *testing.T) []byte {
	t.Helper()
	operator := []byte("kind: Deployment\n")
	sum := sha256.Sum256(operator)
	m := &release.Manifest{
		Version:  "9.0.0",
		Platform: release.PlatformDefaults{ILMVersion: "9.0.0"},
		Compatibility: release.Compatibility{
			Operator: ">=1.0.0", MinAppliance: "0.0.1",
		},
		Addons: []release.AddonFile{{Name: "ilm-operator", File: "ilm-operator.yaml", Version: "v1.1.0", SHA256: hex.EncodeToString(sum[:])}},
	}
	dir := filepath.Join(t.TempDir(), "layout")
	files := fstest.MapFS{"addons/ilm-operator.yaml": {Data: operator}}
	if _, err := bundle.Build(t.Context(), m, release.BundlePlatform, "amd64", bundle.Inputs{Files: files, Notes: []byte("notes")}, dir); err != nil {
		t.Fatal(err)
	}
	bundletest.Sign(t, dir, f.key)
	data, err := os.ReadFile(bundletest.Tar(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// uploadBundle uploads body as a bundle of kind, declaring size bytes; a negative size declares none.
func (h *harness) uploadBundle(kind string, body io.Reader, size int64, s *session) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPut, pathUpdates+"/upload?kind="+kind, body)
	req.ContentLength = size
	req.RemoteAddr = testPeer
	req.Header.Set("Content-Type", "application/octet-stream")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.cookie})
	req.Header.Set(auth.CSRFHeader, s.csrf)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// waitLoad waits for the load in the background to end and returns its state.
func (f *updatesFixture) waitLoad(t *testing.T) update.LoadState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.loader.Last().State == update.StateRunning {
		if time.Now().After(deadline) {
			t.Fatal("the load did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f.loader.Last()
}

// entry is the newest audit entry of action.
func (h *harness) entry(action string) audit.Entry {
	h.t.Helper()
	entries := h.entries()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Action == action {
			return entries[i]
		}
	}
	h.t.Fatalf("no audit entry %s", action)
	return audit.Entry{}
}

func updatesOf(t *testing.T, h *harness, s *session) Updates {
	t.Helper()
	rec := h.request(http.MethodGet, pathUpdates, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var u Updates
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpdatesUnavailable(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	s := h.signIn()
	wantCode(t, h.request(http.MethodGet, pathUpdates, nil, s), http.StatusServiceUnavailable, codeUpdatesUnavailable)
	wantCode(t, h.request(http.MethodPost, pathUpdatesCheck, nil, s), http.StatusServiceUnavailable, codeUpdatesUnavailable)
	wantCode(t, h.uploadBundle("platform", strings.NewReader("x"), 1, s), http.StatusServiceUnavailable, codeUpdatesUnavailable)
	wantCode(t, h.request(http.MethodPost, pathUpdatesFetch, UpdateFetch{Kind: Platform, Version: "1.0.0"}, s),
		http.StatusServiceUnavailable, codeUpdatesUnavailable)
	wantCode(t, h.request(http.MethodGet, pathUpdates+"/notes/platform/1.0.0", nil, s), http.StatusServiceUnavailable, codeUpdatesUnavailable)
}

// TestUploadRefusals refuses an upload without its size, of an unknown kind or without the space for it, and keeps
// nothing of it.
func TestUploadRefusals(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	f := h.withUpdates()
	s := h.signIn()
	wantCode(t, h.uploadBundle("platform", strings.NewReader("data"), -1, s), http.StatusBadRequest, codeInvalid)
	wantCode(t, h.uploadBundle("other", strings.NewReader("data"), 4, s), http.StatusBadRequest, codeInvalid)
	wantCode(t, h.uploadBundle("platform", strings.NewReader("da"), 4, s), http.StatusBadRequest, "incomplete")
	f.noSpace = true
	wantCode(t, h.uploadBundle("platform", strings.NewReader("data"), 4, s), http.StatusConflict, "no_space")
	if entries, _ := os.ReadDir(f.uploads); len(entries) != 0 {
		t.Errorf("the uploads directory keeps %v", entries)
	}
	if st := f.loader.Last(); st.State != "" {
		t.Errorf("a refused upload left the load state %+v", st)
	}
	if e := h.entry(string(AuditEntryActionUpdateUpload)); e.Details["refused"] != "no_space" {
		t.Errorf("audit entry %+v", e)
	}
}

// TestUploadNeedsThreeTimesItsSize refuses an upload unless three times its size is free: the stored file, and the
// two copies its load needs then.
func TestUploadNeedsThreeTimesItsSize(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	h.withUpdates()
	s := h.signIn()
	var expected uint64
	// storage.Check refuses unless twice the expected size is free.
	h.srv.checkSpace = func(_ string, e uint64) error { expected = e; return storage.ErrNoSpace }
	const size = 1001
	wantCode(t, h.uploadBundle("platform", strings.NewReader(strings.Repeat("x", size)), size, s), http.StatusConflict, "no_space")
	if 2*expected < 3*size || 2*expected > 3*size+2 {
		t.Errorf("the upload of %d bytes asked for twice %d bytes free, want three times its size", size, expected)
	}
}

// TestUploadRefusedDuringUpdate refuses an upload while another long operation, such as an update job, holds the
// shared admission, and reads nothing of it.
func TestUploadRefusedDuringUpdate(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	f := h.withUpdates()
	s := h.signIn()
	held, free := make(chan struct{}), make(chan struct{})
	go func() {
		_ = f.runner.Exclusive(func() error { close(held); <-free; return nil })
	}()
	<-held
	body := &countingBody{r: bytes.NewReader(f.signedBundle(t))}
	wantCode(t, h.uploadBundle("platform", body, int64(body.r.Len()), s), http.StatusConflict, codeUpdateBusy)
	wantCode(t, h.request(http.MethodPost, pathUpdatesFetch, UpdateFetch{Kind: Platform, Version: "9.0.0"}, s), http.StatusConflict, codeUpdateBusy)
	close(free)
	if body.n != 0 {
		t.Errorf("%d bytes of a refused upload were read", body.n)
	}
	if entries, _ := os.ReadDir(f.uploads); len(entries) != 0 {
		t.Errorf("the uploads directory keeps %v", entries)
	}
}

// TestReservationRefusesOtherOperationsAtOnce refuses a restore's or another admission while a load is reserved,
// without waiting for the load.
func TestReservationRefusesOtherOperationsAtOnce(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	f := h.withUpdates()
	r, err := f.loader.Reserve(t.Context(), "platform", testUser)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.runner.Exclusive(func() error { return nil }) }()
	select {
	case err := <-done:
		if !errors.Is(err, backup.ErrBusy) {
			t.Errorf("Exclusive while a load is reserved = %v, want ErrBusy", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exclusive waited for the load")
	}
	r.Cancel()
	if err := f.runner.Exclusive(func() error { return nil }); err != nil {
		t.Errorf("Exclusive after the reservation = %v", err)
	}
}

type countingBody struct {
	r *bytes.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestUploadLoadsInTheBackground answers 202 and loads the upload in the background; a refusal shows in the
// updates with its fixed reason, and the upload is removed.
func TestUploadLoadsInTheBackground(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	f := h.withUpdates()
	s := h.signIn()
	body := strings.Repeat("not a bundle", 100)
	wantStatus(t, h.uploadBundle("platform", strings.NewReader(body), int64(len(body)), s), http.StatusAccepted)
	if st := f.waitLoad(t); st.State != update.StateRefused || st.Reason != update.InvalidBundle {
		t.Fatalf("load %+v", st)
	}
	u := updatesOf(t, h, s)
	if u.Load == nil || u.Load.State != BundleLoadStateRefused || u.Load.Reason == nil || *u.Load.Reason != update.InvalidBundle || u.Load.Kind != Platform {
		t.Errorf("load %+v", u.Load)
	}
	if entries, _ := os.ReadDir(f.uploads); len(entries) != 0 {
		t.Errorf("the uploads directory keeps %v", entries)
	}
}

// TestUpdatesListsBundles lists the bundles in the status with the links of their notes, and serves the notes.
func TestUpdatesListsBundles(t *testing.T) {
	h := newHarness(t)
	a := createAppliance(t, v1alpha1.SetupComplete)
	f := h.withUpdates()
	s := h.signIn()
	a.Status.Updates = &v1alpha1.UpdatesStatus{
		LastCheckError: update.Unreachable,
		Available:      []v1alpha1.BundleInfo{{Kind: "system", Version: "1.2.0", Arch: "amd64", SizeBytes: 10, Blocked: update.NeedsAppliance, Needs: "1.1.0"}},
		Loaded:         []v1alpha1.BundleInfo{{Kind: "platform", Version: "1.1.0", Arch: "amd64", ILM: "2.20.0"}},
	}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	u := updatesOf(t, h, s)
	if len(u.Available) != 1 || u.Available[0].Notes != pathUpdates+"/notes/system/1.2.0" || *u.Available[0].Blocked != NeedsAppliance ||
		*u.Available[0].Needs != "1.1.0" || u.LastCheckError == nil || *u.LastCheckError != UpdatesLastCheckErrorUnreachable {
		t.Errorf("updates %+v", u)
	}
	if len(u.Loaded) != 1 || *u.Loaded[0].Ilm != "2.20.0" || u.Load != nil {
		t.Errorf("updates %+v", u)
	}
	notes := filepath.Join(f.dir, "platform", "1.1.0-amd64", "notes.md")
	if err := os.MkdirAll(filepath.Dir(notes), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("# 1.1.0\n<script>"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := h.request(http.MethodGet, pathUpdates+"/notes/platform/1.1.0", nil, s)
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "# 1.1.0\n<script>" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("notes %q, %s", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	wantCode(t, h.request(http.MethodGet, pathUpdates+"/notes/platform/1.2.0", nil, s), http.StatusNotFound, codeNotFound)
}

// TestFetchAnswers refuses a fetch of a version that is not X.Y.Z and loads one in the background otherwise.
func TestFetchAnswers(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	f := h.withUpdates()
	s := h.signIn()
	wantCode(t, h.request(http.MethodPost, pathUpdatesFetch, UpdateFetch{Kind: Platform, Version: "../1"}, s), http.StatusBadRequest, codeInvalid)
	wantStatus(t, h.request(http.MethodPost, pathUpdatesFetch, UpdateFetch{Kind: Platform, Version: "9.0.0"}, s), http.StatusAccepted)
	if st := f.waitLoad(t); st.State != update.StateFailed || st.Version != "9.0.0" {
		t.Errorf("load %+v", st)
	}
	if e := h.entry(update.AuditAction); e.Details["refused"] != update.Failed || e.User != testUser {
		t.Errorf("audit entry %+v", e)
	}
}

// TestCheckAnswers checks the repository and answers the updates with the check's fixed reason.
func TestCheckAnswers(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupComplete)
	h.withUpdates()
	s := h.signIn()
	rec := h.request(http.MethodPost, pathUpdatesCheck, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var u Updates
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.LastCheck == nil || u.LastCheckError == nil || *u.LastCheckError != UpdatesLastCheckErrorFailed {
		t.Errorf("updates %+v", u)
	}
	if e := h.entry(string(AuditEntryActionUpdateCheck)); e.Result != string(Failure) || e.Details[detailRefused] != update.Failed {
		t.Errorf("audit entry %+v", e)
	}
}

// updateAnswers loads a signed bundle whose push fails, fetches and checks online with failures that repeat the
// zot and the registry passwords, and adds the answers to answers.
func updateAnswers(t *testing.T, h *harness, s *session, answers *bytes.Buffer) {
	t.Helper()
	f := h.withUpdates()
	data := f.signedBundle(t)
	rec := h.uploadBundle("platform", bytes.NewReader(data), int64(len(data)), s)
	wantStatus(t, rec, http.StatusAccepted)
	answers.Write(rec.Body.Bytes())
	if st := f.waitLoad(t); st.State != update.StateFailed {
		t.Fatalf("load %+v, want the push to fail", st)
	}
	rec = h.request(http.MethodPost, pathUpdatesFetch, UpdateFetch{Kind: Platform, Version: "9.0.0"}, s)
	wantStatus(t, rec, http.StatusAccepted)
	answers.Write(rec.Body.Bytes())
	f.waitLoad(t)
	rec = h.request(http.MethodPost, pathUpdatesCheck, nil, s)
	wantStatus(t, rec, http.StatusOK)
	answers.Write(rec.Body.Bytes())
}
