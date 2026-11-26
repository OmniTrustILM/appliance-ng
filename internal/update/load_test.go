package update_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle/bundletest"
	"github.com/OmniTrustILM/appliance-ng/internal/registry"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

// TestLoadRejects refuses each bad or unfit bundle with its fixed reason before zot or the bundle directory changes,
// and removes the uploaded file.
func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		// kind is the slot the bundle is loaded into.
		kind string
		// prepare returns the path of the upload.
		prepare func(e *env) string
		want    string
	}{
		{"unsigned", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.key = nil
			return e.upload(e.build(s))
		}, update.Unsigned},
		{"other key", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.key = bundletest.NewKey(e.t)
			return e.upload(e.build(s))
		}, update.BadSignature},
		{"other arch", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.arch = "arm64"
			return e.upload(e.build(s))
		}, update.WrongArch},
		{"truncated", release.BundlePlatform, func(e *env) string {
			return e.truncate(e.upload(e.build(e.platform("1.1.0", "2.20.0"))), 4096)
		}, update.Truncated},
		{"modified image", release.BundlePlatform, func(e *env) string {
			dir := e.build(e.platform("1.1.0", "2.20.0"))
			flipImageLayer(e.t, dir)
			return e.upload(dir)
		}, update.Damaged},
		{"image missing from the index", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.key = nil
			dir := e.build(s)
			rewrite(t, dir, nil, func(im *v1.IndexManifest) { im.Manifests = im.Manifests[:len(im.Manifests)-1] })
			bundletest.Sign(t, dir, e.key)
			return e.upload(dir)
		}, update.Damaged},
		{"image listed with another digest", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.key = nil
			dir := e.build(s)
			rewrite(t, dir, func(md *bundle.Metadata) { md.Images[0].Digest = md.Images[1].Digest }, nil)
			bundletest.Sign(t, dir, e.key)
			return e.upload(dir)
		}, update.Damaged},
		{"file missing from the manifests", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.key = nil
			dir := e.build(s)
			rewrite(t, dir, func(md *bundle.Metadata) { md.Files = append(md.Files, "missing.yaml") }, nil)
			bundletest.Sign(t, dir, e.key)
			return e.upload(dir)
		}, update.InvalidBundle},
		{"file name outside the bundle directory", release.BundlePlatform, func(e *env) string {
			s := e.platform("1.1.0", "2.20.0")
			s.key = nil
			dir := e.build(s)
			rewrite(t, dir, func(md *bundle.Metadata) { md.Files = append(md.Files, "../escape.yaml") }, nil)
			bundletest.Sign(t, dir, e.key)
			return e.upload(dir)
		}, update.InvalidBundle},
		{"version not X.Y.Z", release.BundleSystem, func(e *env) string {
			return e.upload(e.build(e.system("1.2")))
		}, update.InvalidMetadata},
		{"not a bundle", release.BundlePlatform, func(e *env) string {
			return e.writeUpload([]byte(strings.Repeat("not a tar", 200)))
		}, update.InvalidBundle},
		{"system bundle in the platform slot", release.BundlePlatform, func(e *env) string {
			return e.upload(e.build(e.system("1.1.0")))
		}, update.WrongKind},
		{"operator out of range", release.BundleSystem, func(e *env) string {
			s := e.system("1.1.0")
			s.operatorRange = ">=9.0.0"
			return e.upload(e.build(s))
		}, update.OperatorOutOfRange},
		{"platform older than running", release.BundlePlatform, func(e *env) string {
			return e.upload(e.build(e.platform("1.1.0", "2.18.0")))
		}, update.OlderThanRunning},
		{"platform images from a registry the release does not mirror", release.BundlePlatform, func(e *env) string {
			e.rel.Images = []release.Image{{Ref: "ghcr.io/other/tool:1.0"}}
			return e.upload(e.build(e.platform("1.1.0", "2.20.0")))
		}, update.UnmirroredRegistry},
		{"no space", release.BundlePlatform, func(e *env) string {
			e.noSpace = true
			return e.upload(e.build(e.platform("1.1.0", "2.20.0")))
		}, update.NoSpace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			path := c.prepare(e)
			l := e.loader()
			_, err := l.Load(t.Context(), c.kind, path)
			if got := update.Reason(err); got != c.want {
				t.Fatalf("Load = %v (reason %q), want reason %q", err, got, c.want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("the upload is still there: %v", err)
			}
			e.wantUnchanged()
			if st := l.Last(); st.State != update.StateRefused || st.Reason != c.want || st.Kind != c.kind {
				t.Errorf("Last = %+v, want refused with %q", st, c.want)
			}
			entry := e.audit.last()
			if entry.Action != update.AuditAction || entry.Result != "failure" || entry.Details["refused"] != c.want {
				t.Errorf("audit entry %+v", entry)
			}
		})
	}
}

// wantUnchanged fails unless zot, the bundle directory, the status and the uploads directory are as before a load.
func (e *env) wantUnchanged() {
	t := e.t
	t.Helper()
	if repos := e.zotRepositories(); len(repos) != 0 {
		t.Errorf("zot holds %v after a refused load", repos)
	}
	if _, err := os.Stat(e.dir); !os.IsNotExist(err) {
		t.Errorf("the bundle directory exists after a refused load: %v", err)
	}
	if u := e.appliance().Status.Updates; u != nil && len(u.Loaded) != 0 {
		t.Errorf("status lists loaded bundles %+v", u.Loaded)
	}
	if leftovers := filesUnder(t, e.uploads); len(leftovers) != 0 {
		t.Errorf("the uploads directory keeps %v", leftovers)
	}
}

// TestLoadChecksSpaceBeforeOpening asks for twice the tar's size before the tar is extracted.
func TestLoadChecksSpaceBeforeOpening(t *testing.T) {
	e := newEnv(t)
	path := e.upload(e.build(e.platform("1.1.0", "2.20.0")))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.loader().Load(t.Context(), release.BundlePlatform, path); err != nil {
		t.Fatal(err)
	}
	if len(e.spaceChecks) == 0 || e.spaceChecks[0] != uint64(info.Size()) {
		t.Errorf("space checks %v, want the tar's size %d first", e.spaceChecks, info.Size())
	}
}

// TestLoadStoresTheBundle pushes the bundle into zot, stores its files and notes and records it.
func TestLoadStoresTheBundle(t *testing.T) {
	e := newEnv(t)
	path := e.upload(e.build(e.platform("1.1.0", "2.20.0")))
	l := e.loader()
	md, err := l.Load(t.Context(), release.BundlePlatform, path)
	if err != nil {
		t.Fatal(err)
	}
	if md.Version != "1.1.0" || md.ILM != "2.20.0" {
		t.Errorf("metadata %+v", md)
	}
	if got := e.zotBundles(release.BundlePlatform); !slices.Equal(got, []string{"1.1.0-amd64"}) {
		t.Errorf("zot bundles %v", got)
	}
	e.wantFiles(filepath.Join(e.dir, release.BundlePlatform, "1.1.0-amd64"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the upload is still there: %v", err)
	}
	loaded := e.appliance().Status.Updates.Loaded
	if len(loaded) != 1 || loaded[0].Version != "1.1.0" || loaded[0].ILM != "2.20.0" || loaded[0].Operator != testOperator ||
		loaded[0].LoadedAt == nil || loaded[0].SizeBytes == 0 || loaded[0].Blocked != "" {
		t.Errorf("loaded %+v", loaded)
	}
	if st := l.Last(); st.State != update.StateLoaded || st.Version != "1.1.0" {
		t.Errorf("Last = %+v", st)
	}
	if entry := e.audit.last(); entry.Action != update.AuditAction || entry.Result != "success" || entry.Details["version"] != "1.1.0" {
		t.Errorf("audit entry %+v", entry)
	}
	notes, err := l.Notes(release.BundlePlatform, "1.1.0")
	if err != nil || string(notes) != string(testNotes) {
		t.Errorf("Notes = %q, %v", notes, err)
	}
}

// wantFiles fails unless dir holds the platform bundle's manifest and notes.
func (e *env) wantFiles(dir string) {
	e.t.Helper()
	for file, want := range map[string]string{"ilm-operator.yaml": operatorYAML, "notes.md": string(testNotes)} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(data) != want {
			e.t.Errorf("%s = %q, %v", file, data, err)
		}
	}
}

// TestLoadKeepsCurrentAndPrevious keeps the bundle just loaded and the running one, or the newest other one while the
// running version has no bundle, in zot, in the bundle directory and in the status.
func TestLoadKeepsCurrentAndPrevious(t *testing.T) {
	e := newEnv(t)
	l := e.loader()
	load := func(version string) {
		t.Helper()
		if _, err := l.Load(t.Context(), release.BundleSystem, e.upload(e.build(e.system(version)))); err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
	}
	want := func(tags ...string) {
		t.Helper()
		if got := e.zotBundles(release.BundleSystem); !slices.Equal(got, tags) {
			t.Errorf("zot bundles %v, want %v", got, tags)
		}
		if got := e.stored(release.BundleSystem); !slices.Equal(got, tags) {
			t.Errorf("stored bundles %v, want %v", got, tags)
		}
		var listed []string
		for _, b := range e.appliance().Status.Updates.Loaded {
			listed = append(listed, b.Version+"-"+b.Arch)
		}
		slices.Sort(listed)
		if !slices.Equal(listed, tags) {
			t.Errorf("status lists %v, want %v", listed, tags)
		}
	}
	load("1.1.0")
	load("1.2.0")
	want("1.1.0-amd64", "1.2.0-amd64")
	load("1.3.0")
	want("1.2.0-amd64", "1.3.0-amd64")

	e.setVersions(v1alpha1.Versions{Appliance: "1.2.0", Operator: "1.0.0", ILM: "2.19.0"})
	load("1.4.0")
	load("1.5.0")
	want("1.2.0-amd64", "1.5.0-amd64")
	for _, b := range e.appliance().Status.Updates.Loaded {
		if b.Version == "1.2.0" && b.Blocked != update.SameVersion {
			t.Errorf("the running bundle is listed as %q, want %q", b.Blocked, update.SameVersion)
		}
	}
}

// TestLoadKeepsTheWayBack keeps the platform bundle the last update started from while its backup is pinned, and the
// one a rollback installs again until it is done, besides the loaded and the running one.
func TestLoadKeepsTheWayBack(t *testing.T) {
	e := newEnv(t)
	l := e.loader()
	load := func(version, ilm string) {
		t.Helper()
		if _, err := l.Load(t.Context(), release.BundlePlatform, e.upload(e.build(e.platform(version, ilm)))); err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
	}
	want := func(tags ...string) {
		t.Helper()
		if got := e.zotBundles(release.BundlePlatform); !slices.Equal(got, tags) {
			t.Errorf("zot bundles %v, want %v", got, tags)
		}
		if got := e.stored(release.BundlePlatform); !slices.Equal(got, tags) {
			t.Errorf("stored bundles %v, want %v", got, tags)
		}
	}
	load("1.1.0", "2.20.0")
	load("1.2.0", "2.21.0")
	a := e.appliance()
	a.Status.Versions = v1alpha1.Versions{Appliance: "1.0.0", Operator: "1.0.0", ILM: "2.21.0", PlatformBundle: "1.2.0"}
	a.Status.Update = &v1alpha1.UpdateStatus{
		Kind: release.BundlePlatform, From: "1.1.0", FromBundle: "1.1.0", To: "1.2.0", Step: update.StepDone,
		Backup: "pre-update", StartedAt: metav1.Now(),
	}
	if err := e.client.Status().Update(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	e.lock.setBackups(v1alpha1.BackupRecord{Name: "pre-update", Pinned: release.BundlePlatform})
	load("1.3.0", "2.22.0")
	want("1.1.0-amd64", "1.2.0-amd64", "1.3.0-amd64")

	e.lock.setBackups()
	a = e.appliance()
	a.Status.Restore = &v1alpha1.RestoreStatus{
		Backup: "pre-update", InPlace: true, Step: v1alpha1.RestoreStepUninstall, Message: v1alpha1.RestoreFailed,
		Rollback: &v1alpha1.RestoreRollback{ILMVersion: "2.20.0", PlatformBundle: "1.1.0"},
	}
	if err := e.client.Status().Update(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	load("1.4.0", "2.23.0")
	want("1.1.0-amd64", "1.2.0-amd64", "1.4.0-amd64")

	a = e.appliance()
	a.Status.Restore.Step, a.Status.Restore.Message = v1alpha1.RestoreStepDone, ""
	if err := e.client.Status().Update(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	load("1.5.0", "2.24.0")
	want("1.2.0-amd64", "1.5.0-amd64")
}

// TestReserveRefusedWhileAnUpdateIsUnfinished refuses a load while the status shows a platform update that has not
// ended, as after a restart before the update took the admission again.
func TestReserveRefusedWhileAnUpdateIsUnfinished(t *testing.T) {
	e := newEnv(t)
	a := e.appliance()
	a.Status.Update = &v1alpha1.UpdateStatus{
		Kind: release.BundlePlatform, From: "1.0.0", To: "1.1.0", Step: update.StepWait, StartedAt: metav1.Now(),
	}
	if err := e.client.Status().Update(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loader().Reserve(t.Context(), release.BundlePlatform, "admin"); update.Reason(err) != update.Busy {
		t.Errorf("Reserve while an update is unfinished = %v, want busy", err)
	}
	if _, err := e.lock.Hold(); err != nil {
		t.Errorf("the refused load kept the admission: %v", err)
	}
}

// TestReserveIsExclusive refuses a second load while one is reserved and while the shared admission is held.
func TestReserveIsExclusive(t *testing.T) {
	e := newEnv(t)
	l := e.loader()
	r, err := l.Reserve(t.Context(), release.BundlePlatform, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(t.Context(), release.BundlePlatform, "admin"); update.Reason(err) != update.Busy {
		t.Errorf("second Reserve = %v, want busy", err)
	}
	if st := l.Last(); st.State != update.StateRunning {
		t.Errorf("Last while reserved = %+v", st)
	}
	r.Cancel()
	r.Cancel()
	if st := l.Last(); st.State != "" {
		t.Errorf("Last after cancel = %+v, want the state before", st)
	}
	free, err := e.lock.Hold()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(t.Context(), release.BundlePlatform, "admin"); update.Reason(err) != update.Busy {
		t.Errorf("Reserve while the admission is held = %v, want busy", err)
	}
	free()
}

// TestReservationLoadsInTheBackground loads the upload of a reservation and releases the admission afterwards.
func TestReservationLoadsInTheBackground(t *testing.T) {
	e := newEnv(t)
	l := e.loader()
	r, err := l.Reserve(context.Background(), release.BundlePlatform, "admin")
	if err != nil {
		t.Fatal(err)
	}
	done := r.Load(e.upload(e.build(e.platform("1.1.0", "2.20.0"))))
	<-done
	if st := l.Last(); st.State != update.StateLoaded {
		t.Fatalf("Last = %+v", st)
	}
	if entry := e.audit.last(); entry.User != "admin" {
		t.Errorf("audit user %q", entry.User)
	}
	r2, err := l.Reserve(t.Context(), release.BundlePlatform, "admin")
	if err != nil {
		t.Fatalf("Reserve after the load: %v", err)
	}
	r2.Cancel()
}

// stalledRegistry is a registry that never finishes a push.
type stalledRegistry struct{}

func (stalledRegistry) PushBundle(ctx context.Context, _ registry.Source, _ bundle.Metadata, _ func(done, total int64)) error {
	<-ctx.Done()
	return ctx.Err()
}

func (stalledRegistry) Prune(context.Context, string, []string) error { return nil }

// TestLoadTimesOut ends a load into a registry that stalls with the reason timeout and releases the admission.
func TestLoadTimesOut(t *testing.T) {
	e := newEnv(t)
	l := update.NewLoader(update.LoaderDeps{
		Client:    e.client,
		Registry:  func(context.Context) (update.Registry, error) { return stalledRegistry{}, nil },
		Admission: e.lock, Backups: e.lock, Audit: e.audit, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Release: e.rel, Dir: e.dir, Uploads: e.uploads, Key: &e.key.PublicKey, Arch: testArch,
		Space: e.space, LoadTimeout: 300 * time.Millisecond,
	})
	r, err := l.Reserve(t.Context(), release.BundlePlatform, "admin")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Load(e.upload(e.build(e.platform("1.1.0", "2.20.0")))):
	case <-time.After(5 * time.Second):
		t.Fatal("the load did not end")
	}
	if st := l.Last(); st.State != update.StateRefused || st.Reason != update.Timeout {
		t.Errorf("Last = %+v, want refused with timeout", st)
	}
	free, err := e.lock.Hold()
	if err != nil {
		t.Fatalf("the admission is still held: %v", err)
	}
	free()
}

// TestNotesRefusesOtherNames finds notes only by kind and an exact version.
func TestNotesRefusesOtherNames(t *testing.T) {
	l := newEnv(t).loader()
	for _, c := range [][2]string{{"system", "../1.0.0"}, {"other", "1.0.0"}, {"system", "1.0"}, {"system", "9.9.9"}} {
		if _, err := l.Notes(c[0], c[1]); !errors.Is(err, update.ErrNoNotes) {
			t.Errorf("Notes(%q, %q) = %v, want ErrNoNotes", c[0], c[1], err)
		}
	}
}

// flipImageLayer changes a byte of the first layer of the bundle's first image.
func flipImageLayer(t *testing.T, dir string) {
	t.Helper()
	p := layout.Path(dir)
	top, err := p.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := top.ImageIndex(bundletest.IndexDescriptor(t, dir).Digest)
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	img, err := idx.Image(im.Manifests[1].Digest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "blobs", "sha256", m.Layers[0].Digest.Hex)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// rewrite changes the metadata and the bundle index of the unsigned layout in dir, keeping the layout consistent.
func rewrite(t *testing.T, dir string, changeMetadata func(*bundle.Metadata), changeIndex func(*v1.IndexManifest)) {
	t.Helper()
	p := layout.Path(dir)
	desc := bundletest.IndexDescriptor(t, dir)
	var im v1.IndexManifest
	readJSON(t, p, desc.Digest, &im)
	if changeMetadata != nil {
		var m v1.Manifest
		readJSON(t, p, im.Manifests[0].Digest, &m)
		var md bundle.Metadata
		readJSON(t, p, m.Config.Digest, &md)
		changeMetadata(&md)
		m.Config.Digest, m.Config.Size = writeJSON(t, p, md)
		im.Manifests[0].Digest, im.Manifests[0].Size = writeJSON(t, p, m)
	}
	if changeIndex != nil {
		changeIndex(&im)
	}
	digest, size := writeJSON(t, p, im)
	var top v1.IndexManifest
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err == nil {
		err = json.Unmarshal(data, &top)
	}
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range top.Manifests {
		if d.Digest == desc.Digest {
			top.Manifests[i].Digest, top.Manifests[i].Size = digest, size
		}
	}
	if data, err = json.Marshal(top); err == nil {
		err = os.WriteFile(filepath.Join(dir, "index.json"), data, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, p layout.Path, h v1.Hash, v any) {
	t.Helper()
	data, err := p.Bytes(h)
	if err == nil {
		err = json.Unmarshal(data, v)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, p layout.Path, v any) (v1.Hash, int64) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	h, size, err := v1.SHA256(bytes.NewReader(data))
	if err == nil {
		err = p.WriteBlob(h, io.NopCloser(bytes.NewReader(data)))
	}
	if err != nil {
		t.Fatal(err)
	}
	return h, size
}
