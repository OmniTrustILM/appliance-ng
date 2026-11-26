package update

import (
	"context"
	"crypto"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
)

// CheckTimeout bounds a check of the online repository.
const CheckTimeout = 5 * time.Minute

// RecordTimeout bounds the write of a check's result to the status, after the check's own timeout.
const RecordTimeout = 30 * time.Second

var recordTimeout = RecordTimeout

// maxInspected is how many of the newest bundles of a kind newer than what runs a check reads.
const maxInspected = 10

// FinderDeps is what a Finder needs. Key, Arch, Now, Timeout and Space default to the embedded signing key, the
// manager's arch, time.Now, five minutes and storage.Check.
type FinderDeps struct {
	// Client reads the Appliance and writes its status.
	Client  client.Client
	Online  Online
	Release *release.Manifest
	// Dir caches the notes of the bundles found under notes/<kind>/<version>-<arch>.md.
	Dir string
	// Uploads keeps the metadata of the bundles while they are checked.
	Uploads string
	Log     *slog.Logger
	Key     crypto.PublicKey
	Arch    string
	Now     func() time.Time
	Timeout time.Duration
	// Space refuses with storage.ErrNoSpace when the volume at path lacks twice expected bytes free.
	Space func(path string, expected uint64) error
}

// Finder finds the bundles in the online repository that are newer than what runs.
type Finder struct {
	deps FinderDeps
	mu   sync.Mutex
	// running is the check under way; nil while none runs.
	running *check
}

// check is a check under way; err is set once done is closed.
type check struct {
	done chan struct{}
	err  error
}

// NewFinder returns a Finder on deps.
func NewFinder(deps FinderDeps) *Finder {
	if deps.Key == nil {
		deps.Key = bundle.PublicKey()
	}
	if deps.Arch == "" {
		deps.Arch = runtime.GOARCH
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Timeout == 0 {
		deps.Timeout = CheckTimeout
	}
	if deps.Space == nil {
		deps.Space = storage.Check
	}
	return &Finder{deps: deps}
}

// Start checks the repository once a day at a minute chosen at random, until ctx ends.
func (f *Finder) Start(ctx context.Context) error {
	minute := rand.IntN(24 * 60) //nolint:gosec // spreads the appliances' checks; not a secret
	for {
		t := time.NewTimer(UntilMinute(f.deps.Now(), minute))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if err := f.Check(ctx); err != nil {
			f.deps.Log.Info("the daily check for updates failed", "reason", Reason(err))
		}
	}
}

// UntilMinute is the time from now to the next minute of the day, in UTC, a full day when it is now.
func UntilMinute(now time.Time, minute int) time.Duration {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	next := day.Add(time.Duration(minute) * time.Minute)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

// Check lists the bundles of the manager's arch in the online repository that are newer than the running ones,
// verifies each one's signature and metadata and caches its notes, and writes them to status.updates.available.
// A bundle that does not verify is left out and the check reports badSignature; a check that cannot list the
// repository keeps the bundles found before and reports unreachable, timeout or failed. A check asked for while
// another runs waits for that one and returns its result. The check runs on whether or not its callers wait: a caller
// whose ctx ends stops waiting, while the check goes on within its timeout.
func (f *Finder) Check(ctx context.Context) error {
	f.mu.Lock()
	c := f.running
	if c == nil {
		c = &check{done: make(chan struct{})}
		f.running = c
		go f.run(context.WithoutCancel(ctx), c)
	}
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return fail("wait for the check", ctx.Err())
	}
}

// run runs the check c and lets the next check start once it has ended.
func (f *Finder) run(ctx context.Context, c *check) {
	c.err = f.check(ctx)
	f.mu.Lock()
	f.running = nil
	f.mu.Unlock()
	close(c.done)
}

func (f *Finder) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, f.deps.Timeout)
	defer cancel()
	a := &v1alpha1.Appliance{}
	if err := f.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fail("read the appliance", err)
	}
	cur := current(a, f.deps.Release, f.deps.Arch)
	var found []v1alpha1.BundleInfo
	var checkErr error
	var terminal error
	for _, kind := range []string{release.BundleSystem, release.BundlePlatform} {
		infos, skipped, err := f.find(ctx, kind, cur)
		found = append(found, infos...)
		checkErr = errors.Join(checkErr, skipped, err)
		if err != nil {
			terminal = err
			break
		}
	}
	listed := terminal == nil
	reason := finderReason(checkErr)
	if !listed {
		reason = finderReason(terminal)
	}
	record, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancelRecord()
	err := updateStatus(record, f.deps.Client, func(st *v1alpha1.ApplianceStatus) {
		u := updatesOf(st)
		now := metav1.NewTime(f.deps.Now())
		u.LastCheck, u.LastCheckError = &now, reason
		if listed {
			u.Available = found
			f.pruneNotes(found)
		}
	})
	if err != nil {
		return fail("record the check", err)
	}
	if checkErr != nil {
		f.deps.Log.Info("check for updates", "reason", reason, "step", stepOf(checkErr))
		return &Error{Reason: reason, step: stepOf(checkErr), err: checkErr}
	}
	f.deps.Log.Info("checked for updates", "available", len(found))
	return nil
}

// finderReason is the fixed reason of a failed check: badSignature, unreachable, timeout or failed.
func finderReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errSignature) {
		return BadSignature
	}
	if r := Reason(err); r == Unreachable || r == Timeout {
		return r
	}
	return Failed
}

// find returns the verified bundles of kind newer than the running one. A bundle that does not verify or cannot be
// read while the repository answers is left out and its error returned as skipped; err ends the check.
func (f *Finder) find(ctx context.Context, kind string, cur Current) (found []v1alpha1.BundleInfo, skipped, err error) {
	opts, err := f.deps.Online.Options(ctx)
	if err != nil {
		return nil, nil, fail("reach the repository", err)
	}
	repo, err := f.deps.Online.repository(kind)
	if err != nil {
		return nil, nil, fail("name the repository", err)
	}
	tags, err := remote.List(repo, opts...)
	if reasonOf(err) == NotFound {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fail("list the bundles", err)
	}
	var out []v1alpha1.BundleInfo
	tags = newer(tags, f.deps.Arch, runningVersion(cur, kind))
	for _, t := range tags[max(0, len(tags)-maxInspected):] {
		info, err := f.inspect(ctx, kind, t, cur)
		if err != nil && !reachable(err) {
			return out, skipped, err
		}
		if err != nil {
			f.deps.Log.Warn("a bundle in the repository is left out", "kind", kind, "bundle", t, "reason", Reason(err))
			skipped = errors.Join(skipped, err)
			continue
		}
		out = append(out, info)
	}
	return out, skipped, nil
}

// reachable tells whether err leaves the repository reachable and the volume with space, so that the check goes on
// with the other bundles.
func reachable(err error) bool {
	r := Reason(err)
	return errors.Is(err, errSignature) || (r != Unreachable && r != Timeout && r != NoSpace)
}

// newer returns the tags <version>-<arch> whose version is newer than running, oldest first. Every version is newer
// than a running version that is not X.Y.Z.
func newer(tags []string, arch, running string) []string {
	run, err := semver.NewVersion(running)
	var out []string
	for _, t := range tags {
		if v, ok := tagVersion(t, arch); ok && (err != nil || v.GreaterThan(run)) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		va, _ := tagVersion(a, arch)
		vb, _ := tagVersion(b, arch)
		return va.Compare(vb)
	})
	return out
}

// inspect verifies the bundle kind:t, caches its notes and describes it. It reads nothing while the uploads
// directory lacks the space for the most the bundle's metadata and signatures may take.
func (f *Finder) inspect(ctx context.Context, kind, t string, cur Current) (v1alpha1.BundleInfo, error) {
	if err := os.MkdirAll(f.deps.Uploads, dirMode); err != nil {
		return v1alpha1.BundleInfo{}, fail("create the uploads directory", err)
	}
	if err := f.deps.Space(f.deps.Uploads, maxMetadata+maxReferrers*maxSignature); err != nil {
		return v1alpha1.BundleInfo{}, spaceError(err)
	}
	dir, err := os.MkdirTemp(f.deps.Uploads, ".check-")
	if err != nil {
		return v1alpha1.BundleInfo{}, fail("create the uploads directory", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	s, err := f.deps.Online.signed(ctx, kind, t, dir, f.deps.Key, f.deps.Arch)
	if err != nil {
		return v1alpha1.BundleInfo{}, err
	}
	defer func() { _ = s.b.Close() }()
	notes, err := s.b.Notes()
	if err != nil {
		return v1alpha1.BundleInfo{}, fail("read the notes", err)
	}
	path := notesPath(f.deps.Dir, kind, t)
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return v1alpha1.BundleInfo{}, fail("cache the notes", err)
	}
	if err := os.WriteFile(path, notes, fileMode); err != nil {
		return v1alpha1.BundleInfo{}, fail("cache the notes", err)
	}
	infos := []v1alpha1.BundleInfo{bundleInfo(s.md)}
	recheck(infos, cur)
	return infos[0], nil
}

// pruneNotes removes the cached notes of the bundles that are no longer available.
func (f *Finder) pruneNotes(available []v1alpha1.BundleInfo) {
	for _, kind := range []string{release.BundleSystem, release.BundlePlatform} {
		dir := filepath.Dir(notesPath(f.deps.Dir, kind, "x"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			listed := slices.ContainsFunc(available, func(b v1alpha1.BundleInfo) bool {
				return b.Kind == kind && tag(b.Version, b.Arch)+".md" == e.Name()
			})
			if !listed {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
