package update

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/registry"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
)

// States of a load.
const (
	StateRunning = "running"
	StateLoaded  = "loaded"
	StateRefused = "refused"
	StateFailed  = "failed"
)

// AuditAction is the audit action of a finished load.
const AuditAction = "bundle-loaded"

const (
	dirMode      = 0o700
	fileMode     = 0o600
	sourceLocal  = "upload"
	sourceOnline = "online"
)

// ErrNoNotes is returned by Notes for a bundle without stored notes.
var ErrNoNotes = errors.New("no release notes for the bundle")

// Registry is the appliance's registry the loader pushes bundles into.
type Registry interface {
	PushBundle(ctx context.Context, b registry.Source, md bundle.Metadata, progress func(done, total int64)) error
	Prune(ctx context.Context, kind string, keep []string) error
}

// Admission is what every long operation shares: backups, restores, updates and loads.
type Admission interface {
	// Hold marks a long operation as running until release is called, so that the others are refused meanwhile; it
	// refuses at once while another one runs.
	Hold() (release func(), err error)
}

// AuditWriter records the loads in the audit log.
type AuditWriter interface {
	Write(e audit.Entry) error
}

// LoaderDeps is what a Loader needs. Key, Arch, Now, Space, LoadTimeout and FetchTimeout default to the embedded
// signing key, the manager's arch, time.Now, storage.Check, LoadTimeout and FetchTimeout.
type LoaderDeps struct {
	// Client reads the Appliance and writes its status.
	Client client.Client
	// Registry returns a client that pushes into the appliance's registry.
	Registry  func(ctx context.Context) (Registry, error)
	Admission Admission
	Audit     AuditWriter
	Log       *slog.Logger
	// Release is the running release; its operator range is what a system bundle is checked against.
	Release *release.Manifest
	// Dir keeps the verified bundle files in <kind>/<version>-<arch>/ and the cached notes of bundles found online.
	Dir string
	// Uploads keeps the uploaded bundles until they are loaded and the bundles fetched online while they are checked.
	Uploads string
	// Online is where Fetch finds bundles.
	Online Online
	Key    crypto.PublicKey
	Arch   string
	Now    func() time.Time
	Space  func(path string, expected uint64) error
	// LoadTimeout bounds a Load; it defaults to LoadTimeout.
	LoadTimeout time.Duration
	// FetchTimeout bounds a Fetch; it defaults to FetchTimeout.
	FetchTimeout time.Duration
}

func (d *LoaderDeps) defaults() {
	if d.Key == nil {
		d.Key = bundle.PublicKey()
	}
	if d.Arch == "" {
		d.Arch = runtime.GOARCH
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Space == nil {
		d.Space = storage.Check
	}
	if d.LoadTimeout == 0 {
		d.LoadTimeout = LoadTimeout
	}
	if d.FetchTimeout == 0 {
		d.FetchTimeout = FetchTimeout
	}
}

// LoadTimeout bounds the load of an uploaded bundle, which pushes all its images into the registry.
const LoadTimeout = 4 * time.Hour

// FetchTimeout bounds the fetch of a bundle from the online repository, which copies all its images.
const FetchTimeout = 4 * time.Hour

// LoadState is the latest load: running, loaded, refused or failed for a fixed reason.
type LoadState struct {
	State     string
	Kind      string
	Version   string
	Reason    string
	StartedAt time.Time
}

// Loader loads verified bundles into the appliance's registry and its data volume, one at a time.
type Loader struct {
	deps LoaderDeps
	mu   sync.Mutex
	last LoadState
}

// NewLoader returns a Loader on deps.
func NewLoader(deps LoaderDeps) *Loader {
	deps.defaults()
	return &Loader{deps: deps}
}

// Last returns the latest load.
func (l *Loader) Last() LoadState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func (l *Loader) setLast(s LoadState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = s
}

// Reservation holds the shared admission for one load of kind, from the upload or the fetch until the load ends.
type Reservation struct {
	l       *Loader
	kind    string
	user    string
	prev    LoadState
	release func()
	once    sync.Once
	// released is closed once the admission is released.
	released chan struct{}
}

// Reserve takes the shared admission for a load of kind by user, refusing with Busy while another long operation
// runs or a restore is active. The reservation holds it until its Load, Fetch or Cancel ends.
func (l *Loader) Reserve(ctx context.Context, kind, user string) (*Reservation, error) {
	release, err := l.deps.Admission.Hold()
	if err != nil {
		return nil, &Error{Reason: Busy, err: err}
	}
	if err := l.admit(ctx); err != nil {
		release()
		return nil, err
	}
	r := &Reservation{l: l, kind: kind, user: user, release: release, released: make(chan struct{})}
	l.mu.Lock()
	r.prev = l.last
	l.last = LoadState{State: StateRunning, Kind: kind, StartedAt: l.deps.Now()}
	l.mu.Unlock()
	return r, nil
}

// admit refuses a load while a restore is active.
func (l *Loader) admit(ctx context.Context) error {
	a, err := l.appliance(ctx)
	if err != nil {
		return fail("admission", err)
	}
	if a.Status.Restore.Active() {
		return &Error{Reason: Busy}
	}
	return nil
}

// Load loads the bundle at path in the background, within the loader's load timeout, and releases the reservation
// once it ends; the channel is closed once it is released.
func (r *Reservation) Load(path string) <-chan struct{} {
	return r.run(func(ctx context.Context) { _, _ = r.l.Load(ctx, r.kind, path) })
}

// Fetch fetches the bundle version online in the background, within the loader's fetch timeout, and releases the
// reservation once it ends; the channel is closed once it is released.
func (r *Reservation) Fetch(version string) <-chan struct{} {
	return r.run(func(ctx context.Context) { _ = r.l.Fetch(ctx, r.kind, version) })
}

func (r *Reservation) run(load func(ctx context.Context)) <-chan struct{} {
	r.once.Do(func() {
		go func() {
			defer r.end()
			load(withUser(context.Background(), r.user))
		}()
	})
	return r.released
}

// Cancel releases the reservation without a load and restores the latest load's state. After a Load or a Fetch it
// waits for it to end.
func (r *Reservation) Cancel() {
	r.once.Do(func() {
		r.l.setLast(r.prev)
		r.end()
	})
	<-r.released
}

func (r *Reservation) end() {
	r.release()
	close(r.released)
}

type userKey struct{}

func withUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

func userOf(ctx context.Context) string {
	if user, ok := ctx.Value(userKey{}).(string); ok && user != "" {
		return user
	}
	return audit.SystemUser
}

// Load loads the bundle file at path, a tar of its OCI layout, into the slot kind and removes it, whether the bundle
// is loaded or refused. Only a verified, compatible bundle has its images pushed and its files stored; the bundles of
// kind are then pruned to the one loaded and the running or previous one. The load ends after the loader's load
// timeout. The caller holds a reservation. A refusal is an *Error with a fixed reason.
func (l *Loader) Load(ctx context.Context, kind, path string) (bundle.Metadata, error) {
	defer func() { _ = os.RemoveAll(path) }()
	ctx, cancel := context.WithTimeout(ctx, l.deps.LoadTimeout)
	defer cancel()
	l.started(kind, "")
	md, err := l.load(ctx, kind, path)
	l.finish(ctx, kind, sourceLocal, md, err)
	return md, err
}

func (l *Loader) load(ctx context.Context, kind, path string) (bundle.Metadata, error) {
	size, err := sizeOf(path)
	if err != nil {
		return bundle.Metadata{}, fail("read the upload", err)
	}
	if err := l.deps.Space(filepath.Dir(path), uint64(size)); err != nil { //nolint:gosec // a file size is never negative
		return bundle.Metadata{}, spaceError(err)
	}
	b, err := bundle.Open(path)
	if err != nil {
		return bundle.Metadata{}, verifyError(err)
	}
	defer func() { _ = b.Close() }()
	md, err := b.Verify(l.deps.Key, l.deps.Arch)
	if err != nil {
		return bundle.Metadata{}, verifyError(err)
	}
	cur, err := l.admissible(ctx, kind, "", md)
	if err != nil {
		return md, err
	}
	return md, l.install(ctx, b, b, md, cur)
}

// started records a load of kind as running, keeping the start of its reservation.
func (l *Loader) started(kind, version string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := l.last.StartedAt
	if l.last.State != StateRunning {
		start = l.deps.Now()
	}
	l.last = LoadState{State: StateRunning, Kind: kind, Version: version, StartedAt: start}
}

// admissible refuses md unless it is of kind, of version when one is asked for, and installable on what runs.
func (l *Loader) admissible(ctx context.Context, kind, version string, md bundle.Metadata) (Current, error) {
	if md.Kind != kind {
		return Current{}, &Error{Reason: WrongKind}
	}
	if version != "" && md.Version != version {
		return Current{}, &Error{Reason: WrongVersion}
	}
	if _, err := semver.StrictNewVersion(md.Version); err != nil {
		return Current{}, &Error{Reason: InvalidMetadata, err: err}
	}
	a, err := l.appliance(ctx)
	if err != nil {
		return Current{}, fail("read the appliance", err)
	}
	cur := current(a, l.deps.Release, l.deps.Arch)
	if blocked, _ := Check(cur, md); blocked != "" {
		return Current{}, &Error{Reason: blocked}
	}
	return cur, nil
}

// files are the manifests and notes of a verified bundle.
type files interface {
	File(name string) ([]byte, error)
	Notes() ([]byte, error)
}

// install pushes a verified and admitted bundle into the registry, stores its files and prunes and records the
// bundles of its kind.
func (l *Loader) install(ctx context.Context, src registry.Source, f files, md bundle.Metadata, cur Current) error {
	reg, err := l.deps.Registry(ctx)
	if err != nil {
		return fail("reach the registry", err)
	}
	if err := reg.PushBundle(ctx, src, md, nil); err != nil {
		return fail("push", err)
	}
	if err := l.store(f, md); err != nil {
		return fail("store the files", err)
	}
	keep := l.keep(md, cur)
	if err := l.record(ctx, md, keep); err != nil {
		return err
	}
	if err := reg.Prune(ctx, md.Kind, keep); err != nil {
		l.deps.Log.Warn("could not prune the registry", "kind", md.Kind, "keep", keep, "failure", reasonOf(err))
	}
	l.pruneDirs(md.Kind, keep)
	return nil
}

// tag is the name of a bundle in the registry and on the data volume.
func tag(version, arch string) string { return version + "-" + arch }

// store writes the files, the notes and the metadata of md into its bundle directory, replacing an earlier copy.
func (l *Loader) store(f files, md bundle.Metadata) error {
	kindDir := filepath.Join(l.deps.Dir, md.Kind)
	if err := os.MkdirAll(kindDir, dirMode); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(kindDir, partPrefix)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	contents := map[string][]byte{}
	for _, name := range md.Files {
		if contents[name], err = f.File(name); err != nil {
			return err
		}
	}
	if contents[bundle.NotesFile], err = f.Notes(); err != nil {
		return err
	}
	if contents[bundle.MetadataFile], err = json.Marshal(md); err != nil {
		return err
	}
	for name, data := range contents {
		if err := os.WriteFile(filepath.Join(tmp, name), data, fileMode); err != nil {
			return err
		}
	}
	dst := filepath.Join(kindDir, tag(md.Version, md.Arch))
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

const partPrefix = ".part-"

// keep names the bundles of md's kind to keep: md's and the running version's when it is stored, else the newest
// other stored one.
func (l *Loader) keep(md bundle.Metadata, cur Current) []string {
	loaded := tag(md.Version, md.Arch)
	keep := []string{loaded}
	stored := l.storedTags(md.Kind)
	running := tag(runningVersion(cur, md.Kind), l.deps.Arch)
	if running != loaded && slices.Contains(stored, running) {
		return append(keep, running)
	}
	var newest *semver.Version
	other := ""
	for _, t := range stored {
		v, ok := tagVersion(t, l.deps.Arch)
		if t == loaded || !ok || (newest != nil && !v.GreaterThan(newest)) {
			continue
		}
		newest, other = v, t
	}
	if other != "" {
		keep = append(keep, other)
	}
	return keep
}

// storedTags lists the bundle directories of kind.
func (l *Loader) storedTags(kind string) []string {
	entries, err := os.ReadDir(filepath.Join(l.deps.Dir, kind))
	if err != nil {
		return nil
	}
	var tags []string
	for _, e := range entries {
		if e.IsDir() && e.Name()[0] != '.' {
			tags = append(tags, e.Name())
		}
	}
	return tags
}

// pruneDirs removes the bundle directories of kind that are not in keep, and the partial ones a crash left.
func (l *Loader) pruneDirs(kind string, keep []string) {
	partial, _ := filepath.Glob(filepath.Join(l.deps.Dir, kind, partPrefix+"*"))
	for _, p := range partial {
		_ = os.RemoveAll(p)
	}
	for _, t := range l.storedTags(kind) {
		if slices.Contains(keep, t) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(l.deps.Dir, kind, t)); err != nil {
			l.deps.Log.Warn("could not remove a pruned bundle", "kind", kind, "bundle", t)
		}
	}
}

// record lists md as loaded and keeps the other loaded bundles of its kind that are in keep.
func (l *Loader) record(ctx context.Context, md bundle.Metadata, keep []string) error {
	err := updateStatus(ctx, l.deps.Client, func(st *v1alpha1.ApplianceStatus) {
		u := updatesOf(st)
		var loaded []v1alpha1.BundleInfo
		for _, b := range u.Loaded {
			if b.Kind != md.Kind || slices.Contains(keep, tag(b.Version, b.Arch)) && b.Version != md.Version {
				loaded = append(loaded, b)
			}
		}
		info := bundleInfo(md)
		now := metav1.NewTime(l.deps.Now())
		info.LoadedAt = &now
		u.Loaded = append(loaded, info)
		recheck(u.Loaded, current(&v1alpha1.Appliance{Status: *st}, l.deps.Release, l.deps.Arch))
	})
	if err != nil {
		return fail("record the bundle", err)
	}
	return nil
}

// finish records how a load of kind from source ended in its state, the audit log and the log.
func (l *Loader) finish(ctx context.Context, kind, source string, md bundle.Metadata, err error) {
	st := l.Last()
	st.Kind, st.Version, st.State, st.Reason = kind, md.Version, StateLoaded, ""
	e := audit.Entry{
		Time: l.deps.Now().UTC(), User: userOf(ctx), Action: AuditAction, Result: "success",
		Details: map[string]string{"kind": kind, "source": source},
	}
	if md.Version != "" {
		e.Details["version"] = md.Version
	}
	if err != nil {
		st.Reason, st.State = Reason(err), StateRefused
		if st.Reason == Failed {
			st.State = StateFailed
		}
		e.Result, e.Details["refused"] = "failure", st.Reason
		l.deps.Log.Warn("bundle not loaded", "kind", kind, "version", md.Version, "source", source,
			"reason", st.Reason, "step", stepOf(err))
	} else {
		l.deps.Log.Info("bundle loaded", "kind", kind, "version", md.Version, "source", source)
	}
	if werr := l.deps.Audit.Write(e); werr != nil {
		l.deps.Log.Error("could not record the load in the audit log", "kind", kind, "error", werr)
	}
	l.setLast(st)
}

// Notes returns the release notes of the bundle version of kind for the manager's arch: those of a loaded bundle,
// else those cached when the bundle was found online.
func (l *Loader) Notes(kind, version string) ([]byte, error) {
	if !slices.Contains([]string{release.BundleSystem, release.BundlePlatform}, kind) {
		return nil, ErrNoNotes
	}
	if _, err := semver.StrictNewVersion(version); err != nil {
		return nil, ErrNoNotes
	}
	t := tag(version, l.deps.Arch)
	for _, path := range []string{filepath.Join(l.deps.Dir, kind, t, bundle.NotesFile), notesPath(l.deps.Dir, kind, t)} {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // built from a checked kind and version
			return data, nil
		}
	}
	return nil, ErrNoNotes
}

// notesPath is where the notes of a bundle found online are cached.
func notesPath(dir, kind, tag string) string {
	return filepath.Join(dir, "notes", kind, tag+".md")
}

func (l *Loader) appliance(ctx context.Context) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	if err := l.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, err
	}
	return a, nil
}

func sizeOf(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func spaceError(err error) error {
	if errors.Is(err, storage.ErrNoSpace) {
		return &Error{Reason: NoSpace, err: err}
	}
	return fail("check the free space", err)
}
