package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// Steps of an update, in the order they run. Each is recorded in status.update.step before it starts.
const (
	StepBackup   = "backup"
	StepImages   = "images"
	StepOperator = "operator"
	StepVersion  = "version"
	StepWait     = "wait"
	StepDone     = v1alpha1.UpdateStepDone
)

// Reasons an update is refused or fails, besides those of a load and of Check.
const (
	UpdateRunning     = "updateRunning"
	BackupRunning     = "backupRunning"
	RestoreRunning    = "restoreRunning"
	BundleLoading     = "bundleLoading"
	PlatformUpgrading = "platformUpgrading"
	NotInstalled      = "notInstalled"
	BackupFailed      = "backupFailed"
	// BackupIncomplete is a backup before the update that left the database out, as while an external database is
	// unreachable.
	BackupIncomplete = "backupIncomplete"
	// OperatorOlderThanRunning is a platform bundle that would replace the running ILM operator with an older one.
	OperatorOlderThanRunning = "operatorOlderThanRunning"
	BundleUnavailable        = "bundleUnavailable"
	ImagesMissing            = "imagesMissing"
	VersionRejected          = "versionRejected"
	// LastUpdateFailed refuses an update while the last platform update failed past its point of no return and its
	// backup is kept for the rollback.
	LastUpdateFailed = "lastUpdateFailed"
)

// Audit actions of an update: its start, and its end with success or a failure at a step.
const (
	AuditUpdateStart       = "platform-update-start"
	AuditUpdate            = "platform-update"
	AuditSystemUpdateStart = "system-update-start"
	AuditSystemUpdate      = "system-update"
)

// auditActions are the start and end audit actions of each kind of update.
var auditActions = map[string][2]string{
	release.BundlePlatform: {AuditUpdateStart, AuditUpdate},
	release.BundleSystem:   {AuditSystemUpdateStart, AuditSystemUpdate},
}

// Defaults of JobDeps.
const (
	// WaitTimeout bounds the wait for ILM to run the new version with its connectors ready.
	WaitTimeout = 60 * time.Minute
	// OperatorTimeout bounds the wait for the ILM operator of the bundle to roll out.
	OperatorTimeout = 30 * time.Minute
	// HealthTimeout bounds the health check after the reboot into the new Talos, from the reboot on.
	HealthTimeout = 20 * time.Minute
	// ComponentTimeout bounds the wait for a Kubernetes component to run healthy at its new version.
	ComponentTimeout = 10 * time.Minute
	// HandOverTimeout bounds the wait for the manager of the new release to take a system update over.
	HandOverTimeout = 15 * time.Minute
	// RebootRetry is how long a node may keep its boot after a reboot into the new Talos before it is rebooted again.
	RebootRetry = 5 * time.Minute
	defaultPoll = 10 * time.Second
)

// What runs when an update is refused, for the manager and the log.
var (
	errUpdateRunning     = errors.New("another update is running")
	errBackupRunning     = errors.New("a backup is running")
	errRestoreRunning    = errors.New("a restore is running")
	errBundleLoading     = errors.New("a bundle load is running")
	errPlatformUpgrading = errors.New("the ILM operator is upgrading the platform")
	errAdmitting         = errors.New("another operation is being admitted")
	errNoDatabase        = errors.New("the backup does not include the database")

	// errInterrupted is a step the API server's lasting unavailability or the manager's shutdown cut off; the update
	// resumes from its recorded step.
	errInterrupted = errors.New("the update step was interrupted")
)

// Retries of a transient API error: attempts, and the longest delay between two, in poll intervals.
const (
	retryAttempts = 8
	retryCap      = 6
)

// JobBackups is the backup runner: its shared admission and the backups before an update.
type JobBackups interface {
	Admission
	// Busy tells whether a backup runs.
	Busy() bool
	Run(ctx context.Context, reason string, targets []string, opts ...backup.RunOption) (v1alpha1.BackupRecord, error)
	// Unpin releases the pins of kind but that of the backup keep.
	Unpin(kind, keep string) error
	// Local lists the local backups and Open opens one of them, for a rollback.
	Local() ([]v1alpha1.BackupRecord, error)
	Open(name string) (*os.File, int64, error)
}

// ImageTagger tags the images of a bundle in the appliance's registry under their upstream tags.
type ImageTagger interface {
	TagImages(ctx context.Context, images []bundle.ImageRef) error
}

// TalosNode is the Talos API a system update drives.
type TalosNode interface {
	Upgrade(ctx context.Context, image string) error
	RebootPowercycle(ctx context.Context) error
	SetInstallerImage(ctx context.Context, image string) error
	Rollback(ctx context.Context) error
	ExtensionVersion(ctx context.Context, name string) (string, error)
	Versions(ctx context.Context) (talos, kubernetes string, err error)
	SetKubernetesImages(ctx context.Context, component, image string) error
	PullSystemImage(ctx context.Context, image string) error
}

// Applier server-side-applies every document of a multi-document YAML.
type Applier interface {
	Apply(ctx context.Context, docs []byte) error
}

// BundleFetcher loads a bundle from the online repository.
type BundleFetcher interface {
	Fetch(ctx context.Context, kind, version string) error
}

// JobDeps is what Jobs need. Left unset, Arch, Now, Space, Poll, OperatorTimeout, WaitTimeout, HealthTimeout,
// ComponentTimeout and HandOverTimeout take the manager's arch, time.Now, storage.Check and the defaults above.
type JobDeps struct {
	// Client reads and writes the Appliance and reads the Platform and the ILM operator's Deployment.
	Client  client.Client
	Backups JobBackups
	// Restore restores the backup a rollback goes back to.
	Restore Restorer
	// Registry returns a client of the appliance's registry.
	Registry func(ctx context.Context) (ImageTagger, error)
	// Fetcher fetches a bundle that is not loaded.
	Fetcher BundleFetcher
	Audit   AuditWriter
	Log     *slog.Logger
	// Release is the running release, with the embedded ILM operator.
	Release *release.Manifest
	// Dir keeps the verified bundle files, as the loader stores them.
	Dir             string
	Arch            string
	Now             func() time.Time
	Space           func(path string, expected uint64) error
	Poll            time.Duration
	OperatorTimeout time.Duration
	WaitTimeout     time.Duration

	// Version is the manager's own version: a system update runs past its manager step only in the manager of the
	// version it installs.
	Version string
	// Talos returns the Talos API of the node.
	Talos func(ctx context.Context) (TalosNode, error)
	// Manifests applies the manager's manifest of a system bundle.
	Manifests Applier
	// Console tells whether the console answers.
	Console          func(ctx context.Context) error
	HealthTimeout    time.Duration
	ComponentTimeout time.Duration
	HandOverTimeout  time.Duration
	RebootRetry      time.Duration
}

func (d *JobDeps) defaults() {
	loader := LoaderDeps{Arch: d.Arch, Now: d.Now, Space: d.Space}
	loader.defaults()
	d.Arch, d.Now, d.Space = loader.Arch, loader.Now, loader.Space
	if d.Poll == 0 {
		d.Poll = defaultPoll
	}
	if d.OperatorTimeout == 0 {
		d.OperatorTimeout = OperatorTimeout
	}
	if d.WaitTimeout == 0 {
		d.WaitTimeout = WaitTimeout
	}
	if d.HealthTimeout == 0 {
		d.HealthTimeout = HealthTimeout
	}
	if d.ComponentTimeout == 0 {
		d.ComponentTimeout = ComponentTimeout
	}
	if d.HandOverTimeout == 0 {
		d.HandOverTimeout = HandOverTimeout
	}
	if d.RebootRetry == 0 {
		d.RebootRetry = RebootRetry
	}
}

// Jobs runs the updates of the appliance, one at a time, each holding the shared admission of the long operations
// from its start to its end. Its progress is in status.update, from which a restarted manager resumes it.
type Jobs struct {
	deps    JobDeps
	mu      sync.Mutex
	running bool
	// stopped is closed at the manager's shutdown, once KeepResuming runs; the jobs stop at their recorded step then.
	stopped chan struct{}
}

// NewJobs returns Jobs on deps.
func NewJobs(deps JobDeps) *Jobs {
	deps.defaults()
	return &Jobs{deps: deps, stopped: make(chan struct{})}
}

// job is an update this manager runs.
type job struct {
	status v1alpha1.UpdateStatus
	user   string
	// release releases the shared admission.
	release func()
}

func (j *Jobs) begin() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running = true
	return true
}

// start runs jb in the background in ctx without its end, which a request's end must not cut off, until the
// manager's shutdown.
func (j *Jobs) start(ctx context.Context, jb *job) {
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		select {
		case <-j.stopped:
			cancel()
		case <-jobCtx.Done():
		}
	}()
	go func() {
		defer cancel()
		j.run(jobCtx, jb)
	}()
}

func (j *Jobs) end() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
}

// StartPlatform checks that the platform bundle version to may be installed now and installs it in the background,
// started by user. It refuses with an *Error while another long operation runs, while the operator upgrades the
// platform, when the bundle is neither loaded nor found online, cannot be installed, or needs more free space than
// the data volume has.
func (j *Jobs) StartPlatform(ctx context.Context, to, user string) error {
	return j.startUpdate(ctx, user, func(ctx context.Context) (v1alpha1.UpdateStatus, error) { return j.admit(ctx, to) })
}

// startUpdate takes the shared admission, admits the update admit returns and runs it in the background, started by
// user.
func (j *Jobs) startUpdate(ctx context.Context, user string, admit func(ctx context.Context) (v1alpha1.UpdateStatus, error)) error {
	if !j.begin() {
		return &Error{Reason: UpdateRunning, err: errUpdateRunning}
	}
	release, err := j.deps.Backups.Hold()
	if err != nil {
		j.end()
		return j.holdRefusal(ctx, err)
	}
	st, err := admit(ctx)
	if err == nil {
		err = updateStatus(ctx, j.deps.Client, func(s *v1alpha1.ApplianceStatus) { s.Update = &st })
	}
	if err != nil {
		release()
		j.end()
		return err
	}
	j.auditEntry(user, auditActions[st.Kind][0], auditOK, st, "")
	j.deps.Log.Info("update started", "kind", st.Kind, "from", st.From, "to", st.To)
	j.start(ctx, &job{status: st, user: user, release: release})
	return nil
}

// holdRefusal is the refusal of an update the shared admission refused: an operation holds it, which is an update
// while the status shows one unfinished and a bundle load otherwise; a backup runs; or another operation, such as a
// restore, is being admitted.
func (j *Jobs) holdRefusal(ctx context.Context, err error) error {
	if !errors.Is(err, backup.ErrHeld) {
		if j.deps.Backups.Busy() {
			return &Error{Reason: BackupRunning, err: errBackupRunning}
		}
		return &Error{Reason: Busy, err: errAdmitting}
	}
	if a, aerr := j.appliance(ctx); aerr == nil && a.Status.Update.Unfinished() {
		return &Error{Reason: UpdateRunning, err: errUpdateRunning}
	}
	return &Error{Reason: BundleLoading, err: errBundleLoading}
}

// admit refuses an update to the platform bundle to that may not start now and returns its status.
func (j *Jobs) admit(ctx context.Context, to string) (v1alpha1.UpdateStatus, error) {
	a, err := j.appliance(ctx)
	if err != nil {
		return v1alpha1.UpdateStatus{}, fail("read the appliance", err)
	}
	if err := j.refuseBusy(ctx, a); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	if err := j.refuseFailedUpdate(a); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	cur := current(a, j.deps.Release, j.deps.Arch)
	info, ok := target(a.Status.Updates, release.BundlePlatform, to, j.deps.Arch, true)
	if !ok {
		return v1alpha1.UpdateStatus{}, &Error{Reason: NotFound}
	}
	infos := []v1alpha1.BundleInfo{info}
	if recheck(infos, cur); infos[0].Blocked != "" {
		return v1alpha1.UpdateStatus{}, &Error{Reason: infos[0].Blocked}
	}
	if err := j.refuseOperatorDowngrade(a, info); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	if err := j.refuseSpace(info); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	return v1alpha1.UpdateStatus{
		Kind: release.BundlePlatform, From: runningVersion(cur, release.BundlePlatform), FromBundle: cur.platformBundle,
		FromILM: a.Spec.Platform.ILMVersion, To: to, Step: StepBackup, StartedAt: metav1.NewTime(j.deps.Now()),
	}, nil
}

// refuseSpace refuses the bundle info when its size is unknown or the data volume has less than twice its size free.
func (j *Jobs) refuseSpace(info v1alpha1.BundleInfo) error {
	if info.SizeBytes <= 0 {
		return &Error{Reason: InvalidMetadata}
	}
	if err := j.deps.Space(j.deps.Dir, uint64(info.SizeBytes)); err != nil { //nolint:gosec // checked positive
		return spaceError(err)
	}
	return nil
}

// refuseOperatorDowngrade refuses installing the platform bundle info when the add-on pass would then apply an older
// ILM operator than the newer of the one that runs and the one the installed platform bundle applies.
func (j *Jobs) refuseOperatorDowngrade(a *v1alpha1.Appliance, info v1alpha1.BundleInfo) error {
	md, err := InstalledPlatform(j.deps.Dir, a.Status.Versions.PlatformBundle, j.deps.Arch)
	if err != nil {
		return fail("read the platform", err)
	}
	if operatorDowngrade(j.embeddedOperator(), md, &bundle.Metadata{Operator: info.Operator}, a.Status.Versions.Operator) {
		return &Error{Reason: OperatorOlderThanRunning}
	}
	return nil
}

// refuseBusy refuses an update while a restore has not ended, a failed one until it is resumed or abandoned, while
// another update has not ended, while the platform is not installed and while the operator upgrades it.
func (j *Jobs) refuseBusy(ctx context.Context, a *v1alpha1.Appliance) error {
	switch {
	case a.Status.Restore.Unsettled():
		return &Error{Reason: RestoreRunning, err: errRestoreRunning}
	case a.Status.Update.Unfinished():
		return &Error{Reason: UpdateRunning, err: errUpdateRunning}
	case a.Spec.Platform == nil:
		return &Error{Reason: NotInstalled}
	}
	upgrading, err := backup.Upgrading(ctx, j.deps.Client)
	if err != nil {
		return fail("read the platform", err)
	}
	if upgrading {
		return &Error{Reason: PlatformUpgrading, err: errPlatformUpgrading}
	}
	return nil
}

// target is the bundle of kind and version for arch among the loaded bundles, else, when online, among those found
// online.
func target(u *v1alpha1.UpdatesStatus, kind, version, arch string, online bool) (v1alpha1.BundleInfo, bool) {
	if u == nil {
		return v1alpha1.BundleInfo{}, false
	}
	lists := [][]v1alpha1.BundleInfo{u.Loaded}
	if online {
		lists = append(lists, u.Available)
	}
	for _, list := range lists {
		for _, b := range list {
			if b.Kind == kind && b.Version == version && b.Arch == arch {
				return b, true
			}
		}
	}
	return v1alpha1.BundleInfo{}, false
}

// Resume resumes the update a restart cut off at the step its status records, taking the shared admission again.
// Without such an update, or while this manager runs one, it does nothing. It fails while another long operation
// holds the admission or a restore has not ended.
func (j *Jobs) Resume(ctx context.Context) error {
	// Looking first keeps a start from being refused while Resume merely looks.
	if st, err := j.interrupted(ctx); err != nil || st == nil || !j.begin() {
		return err
	}
	st, err := j.interrupted(ctx)
	if err != nil || st == nil {
		j.end()
		return err
	}
	release, err := j.deps.Backups.Hold()
	if err != nil {
		j.end()
		return j.holdRefusal(ctx, err)
	}
	j.deps.Log.Info("update resumed", "kind", st.Kind, "from", st.From, "to", st.To, "step", st.Step)
	j.start(ctx, &job{status: *st, user: audit.SystemUser, release: release})
	return nil
}

// interrupted is the update the status shows unfinished that this manager runs; nil without one, and for a system
// update past its manager step that waits for the manager of the version it installs. It refuses while a restore has
// not ended, which a restart may have cut off as well.
func (j *Jobs) interrupted(ctx context.Context) (*v1alpha1.UpdateStatus, error) {
	a, err := j.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fail("read the appliance", err)
	}
	st := a.Status.Update
	switch {
	case !st.Unfinished() || stepTables[st.Kind] == nil || !j.runs(st):
		return nil, nil
	case a.Status.Restore.Unsettled():
		return nil, &Error{Reason: RestoreRunning, err: errRestoreRunning}
	}
	return st, nil
}

// KeepResuming calls Resume every poll interval until ctx ends: at the manager's start, while a backup holds the
// admission, and whenever an update was left unfinished without a job, as when its status could not be written.
// The end of ctx, the manager's shutdown, stops the jobs at their recorded step. It runs once.
func (j *Jobs) KeepResuming(ctx context.Context) error {
	for {
		if err := j.Resume(ctx); err != nil {
			j.deps.Log.Warn("could not resume the update", "reason", Reason(err), "error", err)
		}
		select {
		case <-ctx.Done():
			close(j.stopped)
			return nil
		case <-time.After(j.deps.Poll):
		}
	}
}

// run runs the steps of jb from its recorded one, recording each before it starts, and releases the admission at
// its end.
func (j *Jobs) run(ctx context.Context, jb *job) {
	defer j.end()
	defer jb.release()
	for jb.status.Step != StepDone {
		step := jb.status.Step
		if !j.runs(&jb.status) {
			j.deps.Log.Info("the update waits for the manager of the new release", "kind", jb.status.Kind,
				"from", jb.status.From, "to", jb.status.To, "step", step, "version", j.deps.Version)
			return
		}
		err := j.do(ctx, jb, step)
		if errors.Is(err, errHandedOver) {
			j.deps.Log.Debug("the update step is with the other manager", "kind", jb.status.Kind, "step", step)
			return
		}
		if errors.Is(err, errInterrupted) || err != nil && ctx.Err() != nil {
			j.deps.Log.Warn("update interrupted; it resumes from its recorded step", "kind", jb.status.Kind, "step", step,
				"error", err)
			return
		}
		if err != nil {
			j.failed(ctx, jb, step, err)
			return
		}
	}
	j.finished(ctx, jb)
}

// do runs step of jb and records the next step.
func (j *Jobs) do(ctx context.Context, jb *job, step string) error {
	table := stepTables[jb.status.Kind]
	run, ok := j.stepFunc(jb, step)
	if !ok {
		return &Error{Reason: Failed, step: step, err: errors.New("unknown update step")}
	}
	st := jb.status
	j.deps.Log.Info("update step started", "kind", st.Kind, "from", st.From, "to", st.To, "step", step)
	if err := run(j, ctx, jb); err != nil {
		return err
	}
	j.deps.Log.Info("update step done", "kind", st.Kind, "from", st.From, "to", st.To, "step", step)
	next := jb.status
	next.Step = table.next(step)
	if next.Step != StepDone {
		if err := j.record(ctx, next); err != nil {
			return err
		}
	}
	jb.status = next
	return nil
}

// failed records the failure of jb at step with its fixed reason; the backup stays pinned for a rollback. Before
// the point of no return, the platform bundle that ran before is recorded again, unless that would downgrade the ILM
// operator the add-on pass applies.
func (j *Jobs) failed(ctx context.Context, jb *job, step string, err error) {
	reason := Reason(err)
	jb.status.Message = reason
	goBack := jb.status.Kind == release.BundlePlatform && !jb.status.PointOfNoReturn && j.keepsOperator(jb.status)
	rerr := j.writeStatus(ctx, func(s *v1alpha1.ApplianceStatus) {
		st := jb.status
		s.Update = &st
		if goBack {
			s.Versions.PlatformBundle = st.FromBundle
		}
	})
	if rerr != nil {
		j.deps.Log.Error("could not record the failed update; it resumes from its recorded step", "kind", jb.status.Kind,
			"step", step, "reason", reason, "error", rerr)
		return
	}
	j.auditEntry(jb.user, auditActions[jb.status.Kind][1], auditFailed, jb.status, step)
	j.deps.Log.Warn("update failed", "kind", jb.status.Kind, "from", jb.status.From, "to", jb.status.To, "step", step,
		"reason", reason, "error", err)
}

// keepsOperator tells whether the platform bundle the update st started from runs the same ILM operator as the one
// it installs, or a newer one. A bundle that is not stored was never recorded, so its operator never ran.
func (j *Jobs) keepsOperator(st v1alpha1.UpdateStatus) bool {
	to, err := InstalledPlatform(j.deps.Dir, st.To, j.deps.Arch)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	from, err := InstalledPlatform(j.deps.Dir, st.FromBundle, j.deps.Arch)
	return err == nil && !older(j.activeOperator(from), j.activeOperator(to))
}

// finished releases the pins of the backups of the update's kind but the update's own and records the update done.
func (j *Jobs) finished(ctx context.Context, jb *job) {
	st := jb.status
	if err := j.deps.Backups.Unpin(st.Kind, st.Backup); err != nil {
		j.deps.Log.Warn("could not release the older pinned backups", "kind", st.Kind, "error", err)
	}
	if err := j.record(ctx, st); err != nil {
		j.deps.Log.Error("could not record the finished update; it resumes from its recorded step", "kind", st.Kind,
			"to", st.To, "error", err)
		return
	}
	j.auditEntry(jb.user, auditActions[st.Kind][1], auditOK, st, "")
	j.deps.Log.Info("update done", "kind", st.Kind, "from", st.From, "to", st.To)
}

func (j *Jobs) record(ctx context.Context, st v1alpha1.UpdateStatus) error {
	return j.writeStatus(ctx, func(s *v1alpha1.ApplianceStatus) { s.Update = &st })
}

// writeStatus applies change to the status, trying again on a transient failure.
func (j *Jobs) writeStatus(ctx context.Context, change func(*v1alpha1.ApplianceStatus)) error {
	if err := j.retry(ctx, func() error { return updateStatus(ctx, j.deps.Client, change) }); err != nil {
		return fail("record the update", err)
	}
	return nil
}

// retry calls op until it succeeds or fails for good, trying a transient API failure again after a delay that grows
// from one poll interval to retryCap of them, retryAttempts times in all. A transient failure that lasts, or the end
// of ctx, is errInterrupted.
func (j *Jobs) retry(ctx context.Context, op func() error) error {
	delay := j.deps.Poll
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || !transient(err) {
			return err
		}
		if attempt == retryAttempts {
			return fmt.Errorf("%w: %w", errInterrupted, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", errInterrupted, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(2*delay, retryCap*j.deps.Poll)
	}
}

// transient tells whether err is a failure to reach the API server or one it reports as temporary.
func transient(err error) bool {
	var nerr net.Error
	return apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsTimeout(err) || apierrors.IsInternalError(err) || apierrors.IsConflict(err) ||
		errors.As(err, &nerr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET)
}

// read reads obj by key, trying again on a transient failure.
func (j *Jobs) read(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	return j.retry(ctx, func() error { return j.deps.Client.Get(ctx, key, obj) })
}

const (
	auditOK     = "success"
	auditFailed = "failure"
)

// auditEntry records action on the update st by user; a failure names the step and its reason.
func (j *Jobs) auditEntry(user, action, result string, st v1alpha1.UpdateStatus, step string) {
	e := audit.Entry{
		Time: j.deps.Now().UTC(), User: user, Action: action, Result: result,
		Details: map[string]string{"kind": st.Kind, "from": st.From, "to": st.To},
	}
	if st.Backup != "" {
		e.Details["backup"] = st.Backup
	}
	if result == auditFailed {
		e.Details["step"], e.Details["reason"] = step, st.Message
	}
	if err := j.deps.Audit.Write(e); err != nil {
		j.deps.Log.Error("could not record the update in the audit log", "action", action, "error", err)
	}
}

func (j *Jobs) appliance(ctx context.Context) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	if err := j.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, err
	}
	return a, nil
}

// poll calls check every poll interval until it is done or fails with an *Error; any other failure is tried again.
// It fails with Timeout after limit.
func (j *Jobs) poll(ctx context.Context, step string, limit time.Duration, check func(ctx context.Context) (bool, error)) error {
	deadline := j.deps.Now().Add(limit)
	for {
		done, err := check(ctx)
		var e *Error
		switch {
		case errors.As(err, &e):
			return err
		case err != nil:
			j.deps.Log.Debug("could not check the update step", "step", step, "error", err)
		case done:
			return nil
		}
		if j.deps.Now().After(deadline) {
			return &Error{Reason: Timeout, step: step}
		}
		select {
		case <-ctx.Done():
			return fail(step, ctx.Err())
		case <-time.After(j.deps.Poll):
		}
	}
}
