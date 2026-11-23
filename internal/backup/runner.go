package backup

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"filippo.io/age"
	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
)

// Why a backup runs.
const (
	RunManual   = "manual"
	RunSchedule = "schedule"
	RunReplace  = "replace"
)

var (
	// ErrBusy refuses a backup while another one runs.
	ErrBusy = errors.New("a backup is running")
	// ErrUpgradeInProgress refuses a backup while the operator upgrades the platform.
	ErrUpgradeInProgress = errors.New("the platform is being upgraded")
	// ErrNoKit refuses a backup until the administrator confirmed the recovery kit that holds its key.
	ErrNoKit = errors.New("no confirmed recovery kit")
	// ErrNoSpace refuses a backup while the data volume lacks twice the size of the last one.
	ErrNoSpace = storage.ErrNoSpace
	// ErrFailed is a backup that broke off; its error names the stage and a fixed reason only.
	ErrFailed = errors.New("the backup failed")
	// ErrNotFound is a local backup or a target the Appliance does not have.
	ErrNotFound = errors.New("no such backup or target")

	errNameTaken = errors.New("a backup of that name exists")
)

// The archive members besides the manifest.
const (
	memberAppliance = "appliance.yaml"
	memberDatabase  = "database.sql"
	secretsDir      = "secrets/"
	databaseManaged = "managed"
)

// Reasons and messages of the BackupHealthy condition; a message names fixed reasons only.
const (
	conditionSucceeded    = "Succeeded"
	conditionLocalFailed  = "LocalFailed"
	conditionTargetFailed = "TargetFailed"
	conditionUpgrade      = "UpgradeInProgress"
	conditionNoKit        = "NoKit"
	messageSucceeded      = "the last backup and its copies succeeded"
	messageUpgrade        = "no backup while the platform is being upgraded"
	messageNoKit          = "no backup until the recovery kit is confirmed"
	reasonNoSpace         = target.ReasonNoSpace
	reasonFailed          = target.ReasonFailed
)

const (
	// stageFile is the stage a failure of the local file names.
	stageFile = "backup file"
	// readApplianceFailed wraps a failed read of the Appliance.
	readApplianceFailed = "read the Appliance: %w"
)

const (
	auditAction  = "backup"
	auditSuccess = "success"
	auditFailure = "failure"
	// finishTimeout bounds the status write that ends a run, which goes on when the run's context ended.
	finishTimeout = 30 * time.Second
	fileMode      = 0o600
	dirMode       = 0o700
)

// Variables so that tests need not wait for a copy to time out.
var (
	// minCopyTimeout is the least time a target copy gets.
	minCopyTimeout = 30 * time.Minute
	// copyRate, in bytes per second, is the slowest transfer a copy may run at once it needs more than
	// minCopyTimeout.
	copyRate int64 = 1 << 20
)

// AuditWriter appends entries to the audit log.
type AuditWriter interface {
	Write(e audit.Entry) error
}

// RunnerDeps is what the Runner needs.
type RunnerDeps struct {
	// Client writes the Appliance status.
	Client client.Client
	// Reader reads past the cache: the Appliance, the Platform and the Secrets; the dumper must read past it too.
	Reader client.Reader
	Dumper Dumper
	Audit  AuditWriter
	// Dir keeps the local backups.
	Dir string
	// Version is the appliance version, written into the file name and the manifest.
	Version string
	// Targets serves the NFS targets: client, configuration, busybox image and namespace. The proxy and the trust
	// bundle are read from the Appliance at each run.
	Targets target.Deps
	Log     *slog.Logger
}

type localFile interface {
	io.Writer
	Sync() error
	Close() error
}

// Runner takes backups, one at a time.
type Runner struct {
	deps RunnerDeps

	// mu guards running and pending.
	mu      sync.Mutex
	running bool
	// pending is a replace or scheduled run that waits for the running backup to end.
	pending *pendingRun

	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	create    func(path string) (localFile, error)
	check     func(dir string, expected uint64) error
	newTarget func(spec v1alpha1.BackupTarget, creds map[string][]byte, deps target.Deps) (target.Target, error)
	scan      func(ctx context.Context, host string, port int32, deps target.Deps) (string, error)
}

// NewRunner returns a Runner on deps.
func NewRunner(deps RunnerDeps) *Runner {
	return &Runner{
		deps: deps, now: time.Now, sleep: sleep, create: createFile, check: storage.Check,
		newTarget: target.New, scan: target.ScanHostKey,
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func createFile(path string) (localFile, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode)
}

// RunOption changes how a backup is recorded.
type RunOption func(*runOptions)

type runOptions struct{ user string }

type pendingRun struct {
	reason  string
	targets []string
	opts    []RunOption
}

// ByUser records the backup as started by user; without it the manager started it.
func ByUser(user string) RunOption {
	return func(o *runOptions) { o.user = user }
}

func runOpts(opts []RunOption) runOptions {
	o := runOptions{user: audit.SystemUser}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Run takes a backup for reason, one of RunManual, RunSchedule and RunReplace, keeps the newest LocalCopies locally
// and copies it to the named targets of the Appliance, each pruned to its retention. A target that fails is
// recorded in the status and fails neither the run nor the other targets. Run refuses with ErrBusy, then
// ErrUpgradeInProgress, ErrNoKit and ErrNoSpace; a backup that breaks off returns ErrFailed and leaves no file.
// A replace or scheduled run refused with ErrBusy starts once the running backup ends, with the background context:
// the backup encrypted to a confirmed new kit must not wait for the next schedule, and a target due must get its copy.
func (r *Runner) Run(ctx context.Context, reason string, targets []string, opts ...RunOption) (v1alpha1.BackupRecord, error) {
	if !r.begin(reason, targets, opts) {
		return v1alpha1.BackupRecord{}, ErrBusy
	}
	defer r.end()
	return r.run(ctx, reason, targets, opts...)
}

// Start makes the checks of Run and, once they pass, takes the backup in the background with the background
// context. It returns the refusals of Run; how the backup ends goes to the status, the audit log and the log.
func (r *Runner) Start(ctx context.Context, reason string, targets []string, opts ...RunOption) error {
	if !r.begin(reason, targets, opts) {
		return ErrBusy
	}
	o := runOpts(opts)
	a, recipient, err := r.admit(ctx, reason, o)
	if err != nil {
		r.end()
		return err
	}
	go func() {
		defer r.end()
		if _, err := r.execute(context.Background(), a, recipient, reason, targets, o); err != nil {
			r.deps.Log.Warn("the backup did not complete", "reason", reason, "failure", Describe(err))
		}
	}()
	return nil
}

// begin marks a run as started, or keeps a replace or scheduled run for later while another one runs.
func (r *Runner) begin(reason string, targets []string, opts []RunOption) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		if reason == RunReplace || reason == RunSchedule {
			r.queue(reason, targets, opts)
		}
		return false
	}
	r.running = true
	return true
}

// queue keeps a run for when the running backup ends. Runs that wait together become one run to all their targets:
// a replace run with the options of the latest replace when one of them is a replace run.
func (r *Runner) queue(reason string, targets []string, opts []RunOption) {
	next := &pendingRun{reason: reason, targets: targets, opts: opts}
	if p := r.pending; p != nil {
		next.targets = slices.Clone(p.targets)
		for _, name := range targets {
			if !slices.Contains(next.targets, name) {
				next.targets = append(next.targets, name)
			}
		}
		if reason != RunReplace {
			next.reason, next.opts = p.reason, p.opts
		}
	}
	r.pending = next
	r.deps.Log.Info("a backup is running; the next one starts when it ends", "reason", next.reason, "targets", next.targets)
}

// end ends a run and starts the run that waits for it.
func (r *Runner) end() {
	r.mu.Lock()
	next := r.pending
	r.pending = nil
	r.running = next != nil
	r.mu.Unlock()
	if next == nil {
		return
	}
	go func() {
		defer r.end()
		if _, err := r.run(context.Background(), next.reason, next.targets, next.opts...); err != nil {
			r.deps.Log.Warn("the waiting backup did not complete", "reason", next.reason, "failure", Describe(err))
		}
	}()
}

// Describe is a fixed description of an error of Run, safe for a log: the refusal or failure it is, or the
// Kubernetes reason of a failed read.
func Describe(err error) string {
	for _, known := range []error{ErrBusy, ErrUpgradeInProgress, ErrNoKit, ErrNoSpace, ErrFailed} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	if reason := apierrors.ReasonForError(err); reason != "" {
		return string(reason)
	}
	return reasonFailed
}

// ResetRunning clears the running backup the status shows while none runs, as after a crash. Without an Appliance
// there is nothing to clear.
func (r *Runner) ResetRunning(ctx context.Context) error {
	a := &v1alpha1.Appliance{}
	err := r.deps.Reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(readApplianceFailed, err)
	}
	if a.Status.Backup == nil || !a.Status.Backup.Running {
		return nil
	}
	return r.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) { backupStatus(st).Running = false })
}

func (r *Runner) run(ctx context.Context, reason string, targets []string, opts ...RunOption) (v1alpha1.BackupRecord, error) {
	o := runOpts(opts)
	a, recipient, err := r.admit(ctx, reason, o)
	if err != nil {
		return v1alpha1.BackupRecord{}, err
	}
	return r.execute(ctx, a, recipient, reason, targets, o)
}

// admit reads the Appliance and the recipient of the backup and records a refusal.
func (r *Runner) admit(ctx context.Context, reason string, o runOptions) (*v1alpha1.Appliance, age.Recipient, error) {
	a, recipient, err := r.prepare(ctx)
	if err != nil && a != nil {
		r.refused(ctx, a, o.user, reason, err)
	}
	return a, recipient, err
}

// execute takes the backup admit allowed.
func (r *Runner) execute(ctx context.Context, a *v1alpha1.Appliance, recipient age.Recipient, reason string, targets []string, o runOptions) (v1alpha1.BackupRecord, error) {
	start, err := r.startTime(ctx, a.Spec.Node.HostName)
	if err != nil {
		return v1alpha1.BackupRecord{}, err
	}
	if err := r.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) { backupStatus(st).Running = true }); err != nil {
		return v1alpha1.BackupRecord{}, err
	}
	r.deps.Log.Info("backup started", "reason", reason, "user", o.user)

	rec, stage, err := r.writeLocal(ctx, a, recipient, start)
	if err != nil {
		failure := localReason(err)
		r.deps.Log.Error("backup failed", "reason", reason, "stage", stage, "failure", failure)
		r.finish(ctx, func(st *v1alpha1.ApplianceStatus) {
			r.refreshLocal(st)
			setHealthy(st, a.Generation, metav1.ConditionFalse, conditionLocalFailed, failure)
		})
		r.audit(o.user, reason, FileName(a.Spec.Node.HostName, start, r.deps.Version), "local: "+failure)
		return v1alpha1.BackupRecord{}, fmt.Errorf("%w while writing the %s: %s", ErrFailed, stage, failure)
	}
	r.deps.Log.Info("backup written", "reason", reason, "name", rec.Name, "sizeBytes", rec.SizeBytes, "database", rec.Database)

	results := r.copyToTargets(ctx, a, rec, targets)
	var failed []string
	for _, res := range results {
		if res.LastError != "" {
			failed = append(failed, res.Name+": "+res.LastError)
		}
	}
	r.finish(ctx, func(st *v1alpha1.ApplianceStatus) {
		b := backupStatus(st)
		b.Last = &rec
		r.refreshLocal(st)
		for _, res := range results {
			setTargetStatus(b, res)
		}
		if len(failed) > 0 {
			setHealthy(st, a.Generation, metav1.ConditionFalse, conditionTargetFailed, strings.Join(failed, ", "))
		} else {
			setHealthy(st, a.Generation, metav1.ConditionTrue, conditionSucceeded, messageSucceeded)
		}
	})
	r.audit(o.user, reason, rec.Name, strings.Join(failed, ", "))
	return rec, nil
}

// startTime is the second the backup is named after: now, or the next second when a backup of this second exists,
// as when a backup follows another within the same second.
func (r *Runner) startTime(ctx context.Context, hostName string) (time.Time, error) {
	for {
		at := r.now().UTC().Truncate(time.Second)
		if _, err := os.Lstat(filepath.Join(r.deps.Dir, FileName(hostName, at, r.deps.Version))); err != nil {
			return at, nil
		}
		if err := r.sleep(ctx, at.Add(time.Second).Sub(r.now())); err != nil {
			return time.Time{}, err
		}
	}
}

// refused records a refusal in the status and the audit log; a failed read is the caller's to report.
func (r *Runner) refused(ctx context.Context, a *v1alpha1.Appliance, user, reason string, err error) {
	var condition, message string
	switch {
	case errors.Is(err, ErrUpgradeInProgress):
		condition, message = conditionUpgrade, messageUpgrade
	case errors.Is(err, ErrNoKit):
		condition, message = conditionNoKit, messageNoKit
	case errors.Is(err, ErrNoSpace):
		condition, message = conditionLocalFailed, reasonNoSpace
	default:
		return
	}
	r.deps.Log.Warn("backup refused", "reason", reason, "refusal", condition)
	r.finish(ctx, func(st *v1alpha1.ApplianceStatus) {
		setHealthy(st, a.Generation, metav1.ConditionFalse, condition, message)
	})
	r.audit(user, reason, "", "refused: "+condition)
}

// prepare reads the Appliance and refuses a backup that may not run; it returns the recipient of the confirmed kit.
// The Appliance comes back with every refusal.
func (r *Runner) prepare(ctx context.Context) (*v1alpha1.Appliance, age.Recipient, error) {
	a := &v1alpha1.Appliance{}
	if err := r.deps.Reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, nil, fmt.Errorf(readApplianceFailed, err)
	}
	upgrading, err := r.upgrading(ctx)
	if err != nil {
		return nil, nil, err
	}
	if upgrading {
		return a, nil, ErrUpgradeInProgress
	}
	recipient, err := r.recipient(ctx, a.Status.RecoveryKit)
	if err != nil {
		return a, nil, err
	}
	records, err := ListLocal(r.deps.Dir)
	if err != nil {
		return a, nil, err
	}
	var last uint64
	if len(records) > 0 {
		last = uint64(records[0].SizeBytes) //nolint:gosec // a file size is never negative
	}
	return a, recipient, r.check(r.deps.Dir, last)
}

// upgrading tells whether the operator reports an upgrade of the Platform; no Platform, or no Platform kind yet,
// is none.
func (r *Runner) upgrading(ctx context.Context) (bool, error) {
	p := &otilm.Platform{}
	err := r.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the Platform: %w", err)
	}
	return p.Status.Upgrade != nil, nil
}

// recipient is the stored backup key, when the administrator confirmed the kit that holds it. A key stored by a
// replacement whose kit was never handed out is held by nobody, so nothing is encrypted to it.
func (r *Runner) recipient(ctx context.Context, st *v1alpha1.RecoveryKitStatus) (age.Recipient, error) {
	stored, err := recovery.StoredKit(ctx, r.deps.Reader)
	if errors.Is(err, recovery.ErrNoRecipient) {
		return nil, ErrNoKit
	}
	if err != nil {
		return nil, err
	}
	if !recovery.Usable(st, stored) {
		return nil, ErrNoKit
	}
	recipient, err := age.ParseX25519Recipient(stored.Recipient)
	if err != nil {
		return nil, fmt.Errorf("the stored backup key does not parse: %w", ErrNoKit)
	}
	return recipient, nil
}

// writeLocal writes the backup as a partial file and renames it once it is complete and on disk, then keeps the
// newest LocalCopies. On failure it removes the partial file and names the stage that failed.
func (r *Runner) writeLocal(ctx context.Context, a *v1alpha1.Appliance, recipient age.Recipient, at time.Time) (v1alpha1.BackupRecord, string, error) {
	rec := v1alpha1.BackupRecord{Name: FileName(a.Spec.Node.HostName, at, r.deps.Version), Time: metav1.NewTime(at)}
	if err := os.MkdirAll(r.deps.Dir, dirMode); err != nil {
		return rec, "backup directory", err
	}
	path := filepath.Join(r.deps.Dir, rec.Name)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return rec, stageFile, errors.Join(errNameTaken, err)
	}
	part := path + partSuffix
	f, err := r.create(part)
	if err != nil {
		return rec, stageFile, err
	}
	out := &countingWriter{w: f}
	stage, err := r.writeArchive(ctx, out, a, recipient, &rec)
	if err == nil {
		stage, err = stageFile, f.Sync()
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		stage, err = stageFile, cerr
	}
	if err == nil {
		err = os.Rename(part, path)
	}
	if err != nil {
		if rerr := os.Remove(part); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			r.deps.Log.Error("could not remove the partial backup", "name", rec.Name+partSuffix)
		}
		return rec, stage, err
	}
	// Renamed, the backup is complete and listed: what fails from here on does not undo it.
	rec.SizeBytes = out.n
	if err := syncDir(r.deps.Dir); err != nil {
		r.deps.Log.Warn("could not sync the backup directory", "failure", localReason(err))
	}
	if err := pruneLocal(r.deps.Dir, LocalCopies, rec.Name); err != nil {
		r.deps.Log.Warn("could not remove the oldest local backups", "failure", localReason(err))
	}
	return rec, "", nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writeArchive writes the Appliance, its Secrets and the database dump to w; it fills rec's database state.
func (r *Runner) writeArchive(ctx context.Context, w io.Writer, a *v1alpha1.Appliance, recipient age.Recipient, rec *v1alpha1.BackupRecord) (string, error) {
	aw, err := NewWriter(w, recipient)
	if err != nil {
		return "archive", err
	}
	spec, err := yaml.Marshal(map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Appliance",
		"metadata": map[string]string{"name": a.Name}, "spec": a.Spec,
	})
	if err != nil {
		return memberAppliance, err
	}
	if err := aw.Add(memberAppliance, int64(len(spec)), strings.NewReader(string(spec))); err != nil {
		return memberAppliance, err
	}
	secrets, err := CollectSecrets(ctx, r.deps.Reader, a)
	if err != nil {
		return "secrets", err
	}
	for i := range secrets {
		data, err := yaml.Marshal(&secrets[i])
		if err != nil {
			return "secrets", err
		}
		name := secretsDir + secrets[i].Namespace + "/" + secrets[i].Name + ".yaml"
		if err := aw.Add(name, int64(len(data)), strings.NewReader(string(data))); err != nil {
			return "secrets", err
		}
	}
	included, err := r.addDatabase(ctx, aw)
	if err != nil {
		return "database", err
	}
	rec.Database = v1alpha1.BackupDatabaseNotIncluded
	if included {
		rec.Database = v1alpha1.BackupDatabaseIncluded
	}
	m := Manifest{
		Appliance:        r.deps.Version,
		ILM:              a.Status.Versions.ILM,
		Operator:         a.Status.Versions.Operator,
		Components:       []string{},
		Database:         databaseManaged,
		DatabaseIncluded: included,
		HostName:         a.Spec.Node.HostName,
		Time:             rec.Time.UTC(),
	}
	if a.Spec.Platform != nil {
		m.ILMHostName = a.Spec.Platform.HostName
		m.Components = append(m.Components, a.Spec.Platform.Components...)
	}
	if err := aw.Close(m); err != nil {
		return "archive", err
	}
	return "", nil
}

// addDatabase streams the dump into the archive. A database that does not answer before the dump starts is left
// out; any other failure fails the backup.
func (r *Runner) addDatabase(ctx context.Context, aw *Writer) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(r.deps.Dumper.Dump(ctx, pw))
	}()
	defer func() {
		_ = pr.CloseWithError(context.Canceled)
		cancel()
		<-done
	}()
	stream := bufio.NewReader(pr)
	if _, err := stream.Peek(1); errors.Is(err, ErrDatabaseUnreachable) {
		r.deps.Log.Warn("the database did not answer; the backup holds no database")
		return false, nil
	}
	return true, aw.Add(memberDatabase, -1, stream)
}

// copyToTargets copies the backup to each named target of the Appliance and prunes the target. Names the Appliance
// no longer has are skipped.
func (r *Runner) copyToTargets(ctx context.Context, a *v1alpha1.Appliance, rec v1alpha1.BackupRecord, names []string) []v1alpha1.TargetStatus {
	if len(names) == 0 || a.Spec.Backup == nil {
		return nil
	}
	deps := r.targetDeps(ctx, a)
	var out []v1alpha1.TargetStatus
	for _, name := range names {
		i := slices.IndexFunc(a.Spec.Backup.Targets, func(t v1alpha1.BackupTarget) bool { return t.Name == name })
		if i < 0 {
			r.deps.Log.Info("backup target no longer exists", "target", name)
			continue
		}
		attempt := metav1.NewTime(r.now().UTC().Truncate(time.Second))
		res := v1alpha1.TargetStatus{Name: name, LastAttempt: &attempt}
		if err := r.copyTo(ctx, a.Spec.Backup.Targets[i], deps, a.Spec.Node.HostName, rec); err != nil {
			res.LastError = target.Reason(err)
			r.deps.Log.Warn("backup copy failed", "target", name, "failure", res.LastError)
		} else {
			res.LastSuccess = &attempt
			r.deps.Log.Info("backup copied", "target", name, "name", rec.Name)
		}
		out = append(out, res)
	}
	return out
}

// targetDeps are the driver dependencies with the proxy and the trusted CAs of a.
func (r *Runner) targetDeps(ctx context.Context, a *v1alpha1.Appliance) target.Deps {
	deps := r.deps.Targets
	deps.Proxy = a.Spec.Proxy
	trust, err := r.trustBundle(ctx, a)
	if err != nil {
		r.deps.Log.Warn("could not read the trusted CAs; the targets trust the system roots only", "failure", target.Reason(err))
	}
	deps.TrustPEM = trust
	return deps
}

// openTarget returns the driver of spec with its credentials.
func (r *Runner) openTarget(ctx context.Context, spec v1alpha1.BackupTarget, deps target.Deps) (target.Target, error) {
	creds, err := r.credentials(ctx, spec)
	if err != nil {
		return nil, err
	}
	return r.newTarget(spec, creds, deps)
}

// copyTo copies the backup to the target spec and prunes it, within copyTimeout of its size; a target that runs
// out of time fails with context.DeadlineExceeded, whatever its driver reports.
func (r *Runner) copyTo(ctx context.Context, spec v1alpha1.BackupTarget, deps target.Deps, hostName string, rec v1alpha1.BackupRecord) error {
	ctx, cancel := context.WithTimeout(ctx, copyTimeout(rec.SizeBytes))
	defer cancel()
	err := r.copyAndPrune(ctx, spec, deps, hostName, rec)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx.Err()
	}
	return err
}

// copyTimeout bounds a copy of size bytes with its pruning: minCopyTimeout, or longer for a backup that needs more
// at copyRate.
func copyTimeout(size int64) time.Duration {
	return max(minCopyTimeout, time.Duration(size/copyRate)*time.Second)
}

func (r *Runner) copyAndPrune(ctx context.Context, spec v1alpha1.BackupTarget, deps target.Deps, hostName string, rec v1alpha1.BackupRecord) error {
	t, err := r.openTarget(ctx, spec, deps)
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(r.deps.Dir, rec.Name))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := t.Put(ctx, rec.Name, f, rec.SizeBytes); err != nil {
		return err
	}
	return prune(ctx, t, int(spec.Retention), hostName, rec.Name)
}

// credentials are the keys of the target's Secret and, for an S3 CA reference, the referenced bundle under CAKey.
func (r *Runner) credentials(ctx context.Context, spec v1alpha1.BackupTarget) (map[string][]byte, error) {
	creds := map[string][]byte{}
	if spec.CredentialsRef != "" {
		s, err := r.systemSecret(ctx, spec.CredentialsRef)
		if err != nil {
			return nil, err
		}
		maps.Copy(creds, s.Data)
	}
	if spec.S3 != nil && spec.S3.CA != nil {
		s, err := r.systemSecret(ctx, spec.S3.CA.Name)
		if err != nil {
			return nil, err
		}
		creds[target.CAKey] = s.Data[spec.S3.CA.Key]
	}
	return creds, nil
}

// trustBundle is the bundle of additional trusted CAs spec.trust references, nil without one.
func (r *Runner) trustBundle(ctx context.Context, a *v1alpha1.Appliance) ([]byte, error) {
	if a.Spec.Trust == nil {
		return nil, nil
	}
	s, err := r.systemSecret(ctx, a.Spec.Trust.Name)
	if err != nil {
		return nil, err
	}
	return s.Data[a.Spec.Trust.Key], nil
}

func (r *Runner) systemSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := r.deps.Reader.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: name}, s); err != nil {
		return nil, fmt.Errorf("read secret %s/%s: %w", kube.SystemNamespace, name, err)
	}
	return s, nil
}

// prune deletes all but the newest keep backups of the appliance hostName on t, never the backup written; the
// backups of other appliances that share the location stay.
func prune(ctx context.Context, t target.Target, keep int, hostName, written string) error {
	objects, err := t.List(ctx)
	if err != nil {
		return err
	}
	objects = slices.DeleteFunc(objects, func(o target.Object) bool {
		return o.Name == written || !ownBackup(o.Name, hostName)
	})
	slices.SortFunc(objects, func(x, y target.Object) int {
		return newestFirst(
			v1alpha1.BackupRecord{Name: x.Name, Time: metav1.NewTime(fileTime(x.Name, x.Modified))},
			v1alpha1.BackupRecord{Name: y.Name, Time: metav1.NewTime(fileTime(y.Name, y.Modified))})
	})
	for _, o := range objects[min(max(keep-1, 0), len(objects)):] {
		if err := t.Delete(ctx, o.Name); err != nil {
			return err
		}
	}
	return nil
}

// ownBackup tells whether name is a backup file of the appliance hostName: the host name, then the time. A host
// name that merely starts with hostName and a dash, as appliance-2 does for appliance, is another appliance.
func ownBackup(name, hostName string) bool {
	rest, ok := strings.CutPrefix(name, hostName+"-")
	if !ok || len(rest) <= len(timeLayout) || rest[len(timeLayout)] != '-' {
		return false
	}
	_, err := time.Parse(timeLayout, rest[:len(timeLayout)])
	return err == nil
}

// localReason is the fixed reason of a local failure: noSpace when the volume is full, failed otherwise.
func localReason(err error) string {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return reasonNoSpace
	}
	return reasonFailed
}

// refreshLocal lists the local backups into the status, keeping the database state recorded for each.
func (r *Runner) refreshLocal(st *v1alpha1.ApplianceStatus) {
	b := backupStatus(st)
	records, err := ListLocal(r.deps.Dir)
	if err != nil {
		r.deps.Log.Warn("could not list the local backups", "failure", localReason(err))
		return
	}
	known := map[string]string{}
	for _, rec := range b.Local {
		known[rec.Name] = rec.Database
	}
	if b.Last != nil {
		known[b.Last.Name] = b.Last.Database
	}
	for i := range records {
		if db, ok := known[records[i].Name]; ok {
			records[i].Database = db
		}
	}
	b.Local = records
}

func backupStatus(st *v1alpha1.ApplianceStatus) *v1alpha1.BackupStatus {
	if st.Backup == nil {
		st.Backup = &v1alpha1.BackupStatus{}
	}
	return st.Backup
}

func setTargetStatus(b *v1alpha1.BackupStatus, res v1alpha1.TargetStatus) {
	i := slices.IndexFunc(b.Targets, func(t v1alpha1.TargetStatus) bool { return t.Name == res.Name })
	if i < 0 {
		b.Targets = append(b.Targets, v1alpha1.TargetStatus{Name: res.Name})
		i = len(b.Targets) - 1
	}
	t := &b.Targets[i]
	t.LastAttempt, t.LastError = res.LastAttempt, res.LastError
	if t.FirstAttempt == nil {
		t.FirstAttempt = res.LastAttempt
	}
	if res.LastSuccess != nil {
		t.LastSuccess = res.LastSuccess
	}
}

func setHealthy(st *v1alpha1.ApplianceStatus, generation int64, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionBackupHealthy, Status: status, Reason: reason, Message: message, ObservedGeneration: generation,
	})
}

// updateStatus applies change to the current status of the Appliance and writes it.
func (r *Runner) updateStatus(ctx context.Context, change func(*v1alpha1.ApplianceStatus)) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a := &v1alpha1.Appliance{}
		if err := r.deps.Reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
			return err
		}
		change(&a.Status)
		return r.deps.Client.Status().Update(ctx, a, client.FieldOwner(kube.ManagerFieldManager))
	})
	if err != nil {
		return fmt.Errorf("update the backup status: %w", err)
	}
	return nil
}

// finish ends a run in the status, even when the run's context ended; a failure is logged.
func (r *Runner) finish(ctx context.Context, change func(*v1alpha1.ApplianceStatus)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	err := r.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) {
		change(st)
		backupStatus(st).Running = false
	})
	if err != nil {
		r.deps.Log.Error("could not record the backup in the status", "failure", apierrors.ReasonForError(err))
	}
}

// audit records a run; failed lists what failed with its fixed reason, empty for a run that fully succeeded.
func (r *Runner) audit(user, reason, name, failed string) {
	e := audit.Entry{
		Time: r.now().UTC(), User: user, Action: auditAction, Result: auditSuccess,
		Details: map[string]string{"name": name, "reason": reason},
	}
	if name == "" {
		delete(e.Details, "name")
	}
	if failed != "" {
		e.Result = auditFailure
		e.Details["failed"] = failed
	}
	if err := r.deps.Audit.Write(e); err != nil {
		r.deps.Log.Error("could not record the backup in the audit log", "name", name, "error", err)
	}
}

// Local lists the local backups, newest first.
func (r *Runner) Local() ([]v1alpha1.BackupRecord, error) {
	return ListLocal(r.deps.Dir)
}

// Open opens the local backup name for reading and returns its size; ErrNotFound when no local backup has that
// name. Only names the listing returns are opened, so name cannot reach outside the backup directory.
func (r *Runner) Open(name string) (*os.File, int64, error) {
	records, err := ListLocal(r.deps.Dir)
	if err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(records, func(rec v1alpha1.BackupRecord) bool { return rec.Name == name })
	if i < 0 {
		return nil, 0, ErrNotFound
	}
	f, err := os.Open(filepath.Join(r.deps.Dir, records[i].Name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// TestTarget writes, reads back and deletes a test file on the target name of the Appliance; ErrNotFound when it
// has no such target. The error maps to a fixed reason with target.Reason.
func (r *Runner) TestTarget(ctx context.Context, name string) error {
	a := &v1alpha1.Appliance{}
	if err := r.deps.Reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return fmt.Errorf(readApplianceFailed, err)
	}
	if a.Spec.Backup == nil {
		return ErrNotFound
	}
	i := slices.IndexFunc(a.Spec.Backup.Targets, func(t v1alpha1.BackupTarget) bool { return t.Name == name })
	if i < 0 {
		return ErrNotFound
	}
	t, err := r.openTarget(ctx, a.Spec.Backup.Targets[i], r.targetDeps(ctx, a))
	if err != nil {
		return err
	}
	err = target.Test(ctx, t)
	r.deps.Log.Info("backup target tested", "target", name, "result", cmp.Or(target.Reason(err), "ok"))
	return err
}

// ScanHostKey returns the SHA256 fingerprint of the SSH host key at host:port, reached through the proxy of the
// Appliance as SFTP targets are.
func (r *Runner) ScanHostKey(ctx context.Context, host string, port int32) (string, error) {
	a := &v1alpha1.Appliance{}
	if err := r.deps.Reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return "", fmt.Errorf(readApplianceFailed, err)
	}
	return r.scan(ctx, host, port, r.targetDeps(ctx, a))
}
