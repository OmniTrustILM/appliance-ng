package restore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"filippo.io/age"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Refusals of Start and Resume.
var (
	// ErrBusy refuses a restore while another one runs or waits for its kit.
	ErrBusy = errors.New("a restore is in progress")
	// ErrNotWaiting refuses Resume when no restore waits for its kit or stopped on a failure.
	ErrNotWaiting = errors.New("no restore waits to be resumed")
	// ErrSetup refuses a fresh restore on an appliance that is not set up to restore, and a restore in place on one
	// whose setup is not complete.
	ErrSetup = errors.New("the appliance's setup does not allow this restore")
	// ErrUpgradeInProgress refuses a restore while the operator upgrades the platform.
	ErrUpgradeInProgress = backup.ErrUpgradeInProgress
	// ErrBackupRunning refuses a restore while a backup runs.
	ErrBackupRunning = errors.New("a backup is running")

	errTimeout = errors.New("timed out")
)

const (
	// attempts is how often a step runs before the restore stops on its failure.
	attempts     = 3
	auditAction  = "restore"
	auditSuccess = "success"
	auditFailure = "failure"
	// finishTimeout bounds the status writes that end a run.
	finishTimeout = 30 * time.Second
)

// The steps of a fresh restore and of a restore in place, done excluded.
var (
	freshSteps = []string{
		v1alpha1.RestoreStepSecrets, v1alpha1.RestoreStepAppliance, v1alpha1.RestoreStepPlatform, v1alpha1.RestoreStepStop,
		v1alpha1.RestoreStepILMSecrets, v1alpha1.RestoreStepDatabase, v1alpha1.RestoreStepStart, v1alpha1.RestoreStepKeycloak,
		v1alpha1.RestoreStepCertificate,
	}
	inPlaceSteps = []string{
		v1alpha1.RestoreStepSecrets, v1alpha1.RestoreStepAppliance, v1alpha1.RestoreStepStop, v1alpha1.RestoreStepILMSecrets,
		v1alpha1.RestoreStepDatabase, v1alpha1.RestoreStepStart,
	}
)

// Options are the administrator's choices for a restore.
type Options struct {
	// InPlace restores onto the appliance the backup is from, or one set up before, instead of a fresh one.
	InPlace bool
	// ILMHostName replaces the backup's ILM host name; empty keeps it.
	ILMHostName string
	// User is the console account that starts the restore.
	User string
}

// BackupRunner admits a restore apart from the backups: Exclusive runs admit while no backup runs or passes its
// checks, and refuses with backup.ErrBusy while one runs.
type BackupRunner interface {
	Exclusive(admit func() error) error
}

// AuditWriter appends entries to the audit log.
type AuditWriter interface {
	Write(e audit.Entry) error
}

// Deps is what the Engine needs.
type Deps struct {
	// Client reads past the cache and writes: the Appliance, Secrets, Deployments and the operator's kinds.
	Client  client.Client
	Dumper  backup.Dumper
	Backups BackupRunner
	Audit   AuditWriter
	Log     *slog.Logger
	// OperatorLock, when set, is held while the ILM operator is scaled; the add-on pass holds it while it applies the
	// operator's Deployment.
	OperatorLock sync.Locker
	// KeycloakURL is the base URL of the managed Keycloak; empty is the cluster IP of its Service.
	KeycloakURL string
	// HTTPClient calls Keycloak; nil is a client that never uses a proxy.
	HTTPClient *http.Client
}

// Engine restores a backup onto the appliance, one restore at a time and step by step. Each step is recorded in the
// Appliance status before it runs and can run again, so a restore resumes at the step it was at. The backup key
// lives only in memory: a restore cut off by a restart waits at its step until Resume gets the key again.
type Engine struct {
	deps  Deps
	steps map[string]func(context.Context, *job) error

	// mu guards running.
	mu      sync.Mutex
	running bool
	wg      sync.WaitGroup

	retryDelay  time.Duration
	poll        time.Duration
	waitTimeout time.Duration
	now         func() time.Time
	// afterStep, when set, is called after each step; true ends the run there, as a crash would.
	afterStep func(step string) bool
}

// job is the run of one step.
type job struct {
	id     age.Identity
	status v1alpha1.RestoreStatus
}

// New returns an Engine on deps.
func New(deps Deps) *Engine {
	if deps.HTTPClient == nil {
		deps.HTTPClient = keycloakHTTPClient()
	}
	e := &Engine{deps: deps, retryDelay: 10 * time.Second, poll: 2 * time.Second, waitTimeout: 30 * time.Minute, now: time.Now}
	e.steps = map[string]func(context.Context, *job) error{
		v1alpha1.RestoreStepSecrets:     e.restoreSystemSecrets,
		v1alpha1.RestoreStepAppliance:   e.restoreAppliance,
		v1alpha1.RestoreStepPlatform:    e.waitForPlatform,
		v1alpha1.RestoreStepStop:        e.stop,
		v1alpha1.RestoreStepILMSecrets:  e.restoreILMSecrets,
		v1alpha1.RestoreStepDatabase:    e.restoreDatabase,
		v1alpha1.RestoreStepStart:       e.start,
		v1alpha1.RestoreStepKeycloak:    e.rehostKeycloak,
		v1alpha1.RestoreStepCertificate: e.restoreCertificate,
	}
	return e
}

// Start checks that the restore may run and verifies the whole backup with the backup key id, records the restore,
// then restores file in the background. The backup must have passed Inspect with id. It refuses with ErrBusy,
// ErrSetup, ErrUpgradeInProgress, ErrBackupRunning and the refusals of a backup that does not verify; how the
// restore ends goes to the status, the audit log and the log.
func (e *Engine) Start(ctx context.Context, file string, id age.Identity, opts Options) error {
	if !e.begin() {
		return ErrBusy
	}
	if err := e.admit(ctx, file, id, opts); err != nil {
		e.end()
		return err
	}
	e.deps.Log.Info("restore started", "backup", filepath.Base(file), "inPlace", opts.InPlace)
	e.launch(id)
	return nil
}

// Resume continues a restore that waits for its kit, or one that stopped on a failure, at its step. It verifies the
// whole backup with id first and refuses with ErrNotWaiting when there is nothing to resume.
func (e *Engine) Resume(ctx context.Context, id age.Identity) error {
	if !e.begin() {
		return ErrBusy
	}
	if err := e.resumable(ctx, id); err != nil {
		e.end()
		return err
	}
	e.launch(id)
	return nil
}

// Recover marks a restore that a restart cut off as waiting for its kit, keeping the step it resumes at. It is
// called once when the manager starts; a restore that completed or failed is left as it is.
func (e *Engine) Recover(ctx context.Context) error {
	a := &v1alpha1.Appliance{}
	err := e.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the Appliance: %w", err)
	}
	st := a.Status.Restore
	if !st.Active() || st.Step == v1alpha1.RestoreStepWaitingForKit {
		return nil
	}
	e.deps.Log.Info("the restore waits for the recovery kit", "backup", st.Backup, "step", st.Step)
	return e.updateStatus(ctx, func(s *v1alpha1.ApplianceStatus) {
		s.Restore.PausedStep, s.Restore.Step = s.Restore.Step, v1alpha1.RestoreStepWaitingForKit
	})
}

// KeepRecovering calls Recover until it succeeds or ctx ends: a restore that a restart cut off and that is not marked
// waiting for its kit cannot be resumed. It stops once this process runs a restore, whose status is its own.
func (e *Engine) KeepRecovering(ctx context.Context) {
	for {
		if e.isRunning() {
			return
		}
		err := e.Recover(ctx)
		if err == nil {
			return
		}
		e.deps.Log.Warn("could not mark the interrupted restore as waiting for its kit", "reason", reason(err))
		if sleep(ctx, e.retryDelay) != nil {
			return
		}
	}
}

func (e *Engine) begin() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return false
	}
	e.running = true
	return true
}

func (e *Engine) isRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

func (e *Engine) end() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = false
}

func (e *Engine) launch(id age.Identity) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.end()
		e.run(context.Background(), id)
	}()
}

// admit refuses a restore that may not start and records the one that may.
func (e *Engine) admit(ctx context.Context, file string, id age.Identity, opts Options) error {
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	if a.Status.Restore.Active() {
		return ErrBusy
	}
	want := v1alpha1.SetupRestore
	if opts.InPlace {
		want = v1alpha1.SetupComplete
	}
	if a.Status.Setup != want {
		return ErrSetup
	}
	// The whole file is read before the quick checks, which then stay right before the restore is recorded.
	if _, err := verify(ctx, file, id); err != nil {
		return err
	}
	upgrading, err := backup.Upgrading(ctx, e.deps.Client)
	if err != nil {
		return err
	}
	if upgrading {
		return ErrUpgradeInProgress
	}
	err = e.deps.Backups.Exclusive(func() error {
		return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) {
			st.Restore = &v1alpha1.RestoreStatus{
				Backup: filepath.Base(file), File: file, InPlace: opts.InPlace, ILMHostName: opts.ILMHostName,
				Step: v1alpha1.RestoreStepSecrets, StartedAt: metav1.NewTime(e.now()), StartedBy: opts.User,
			}
		})
	})
	if errors.Is(err, backup.ErrBusy) {
		return ErrBackupRunning
	}
	return err
}

// resumable checks that a restore waits to be resumed and that id opens its backup, and records it as running.
func (e *Engine) resumable(ctx context.Context, id age.Identity) error {
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	st := a.Status.Restore
	if st == nil || st.Step == v1alpha1.RestoreStepDone || (st.Step != v1alpha1.RestoreStepWaitingForKit && st.Message == "") {
		return ErrNotWaiting
	}
	if _, err := verify(ctx, st.File, id); err != nil {
		return err
	}
	e.deps.Log.Info("restore resumed", "backup", st.Backup, "step", st.CurrentStep())
	return e.updateStatus(ctx, func(s *v1alpha1.ApplianceStatus) {
		s.Restore.Step, s.Restore.PausedStep, s.Restore.Message = s.Restore.CurrentStep(), "", ""
	})
}

// run runs the recorded step and the ones after it until the restore is done or fails. A status that cannot be read
// or written fails the restore like a failing step, so that the operator is started again.
func (e *Engine) run(ctx context.Context, id age.Identity) {
	j := &job{id: id}
	for {
		var st *v1alpha1.RestoreStatus
		err := e.retried(ctx, func() error {
			a, err := e.appliance(ctx)
			if err == nil {
				st = a.Status.Restore
			}
			return err
		})
		if err != nil {
			e.fail(ctx, j, err)
			return
		}
		if !st.Active() {
			return
		}
		j = &job{id: id, status: *st}
		if err := e.attempt(ctx, j); err != nil {
			e.fail(ctx, j, err)
			return
		}
		if e.afterStep != nil && e.afterStep(st.Step) {
			return
		}
		if err := e.retried(ctx, func() error { return e.advance(ctx, j) }); err != nil {
			e.fail(ctx, j, err)
			return
		}
	}
}

// retried calls fn up to attempts times, until it succeeds.
func (e *Engine) retried(ctx context.Context, fn func() error) error {
	var err error
	for n := 1; n <= attempts; n++ {
		if err = fn(); err == nil {
			return nil
		}
		e.deps.Log.Warn("restore status not accessible", "attempt", n, "reason", reason(err))
		if n < attempts {
			if serr := sleep(ctx, e.retryDelay); serr != nil {
				return serr
			}
		}
	}
	return err
}

// attempt runs the job's step up to attempts times; a step the engine does not know fails at once.
func (e *Engine) attempt(ctx context.Context, j *job) error {
	step := j.status.Step
	run := e.steps[step]
	if run == nil {
		return fmt.Errorf("no restore step %q", step)
	}
	var err error
	for n := 1; n <= attempts; n++ {
		e.deps.Log.Info("restore step started", "step", step, "attempt", n)
		if err = run(ctx, j); err == nil {
			return nil
		}
		e.deps.Log.Warn("restore step failed", "step", step, "attempt", n, "reason", reason(err))
		if n < attempts {
			if serr := sleep(ctx, e.retryDelay); serr != nil {
				return serr
			}
		}
	}
	return err
}

// advance records the step after the job's, or completes the restore after its last step.
func (e *Engine) advance(ctx context.Context, j *job) error {
	steps := freshSteps
	if j.status.InPlace {
		steps = inPlaceSteps
	}
	i := slices.Index(steps, j.status.Step)
	if i+1 < len(steps) {
		return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) { st.Restore.Step = steps[i+1] })
	}
	return e.finish(ctx, j)
}

// finish completes the restore: setup is complete, a fresh restore counts as a restore test, and the audit log
// records it.
func (e *Engine) finish(ctx context.Context, j *job) error {
	now := metav1.NewTime(e.now())
	err := e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) {
		st.Restore.Step = v1alpha1.RestoreStepDone
		st.Setup = v1alpha1.SetupComplete
		if !j.status.InPlace {
			st.LastRestoreTest = &now
		}
	})
	if err != nil {
		return err
	}
	e.deps.Log.Info("restore done", "backup", j.status.Backup, "inPlace", j.status.InPlace)
	e.audit(j, auditSuccess, nil)
	return nil
}

// fail stops the restore at the job's step with a fixed reason and starts the operator again, so that a restore that
// cannot finish never leaves ILM stopped. The operator is started first, whatever the hold says; a failure that
// cannot be recorded now is recorded in the background once the status can be written, which releases the hold.
func (e *Engine) fail(ctx context.Context, j *job, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	message := v1alpha1.RestoreFailed
	if errors.Is(err, errTimeout) {
		message = v1alpha1.RestoreTimeout
	}
	e.deps.Log.Error("restore failed", "backup", j.status.Backup, "step", j.status.Step, "reason", message)
	if serr := e.scaleOperator(ctx, 1); serr != nil {
		e.deps.Log.Error("could not start the ILM operator again", "reason", reason(serr))
	}
	record := func(ctx context.Context) error {
		return e.updateStatus(ctx, func(st *v1alpha1.ApplianceStatus) {
			if st.Restore != nil {
				st.Restore.Message = message
			}
		})
	}
	if serr := e.retried(ctx, func() error { return record(ctx) }); serr != nil {
		e.deps.Log.Error("could not record the failed restore; trying again in the background", "reason", reason(serr))
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.keepTrying(context.WithoutCancel(ctx), record)
		}()
	}
	e.audit(j, auditFailure, map[string]string{"step": j.status.Step, "failed": message})
}

// keepTrying calls fn until it succeeds or ctx ends, the retry delay apart.
func (e *Engine) keepTrying(ctx context.Context, fn func(context.Context) error) {
	for fn(ctx) != nil {
		if sleep(ctx, e.retryDelay) != nil {
			return
		}
	}
}

// audit records the end of a restore; the entry names the backup, never its key or content.
func (e *Engine) audit(j *job, result string, details map[string]string) {
	entry := audit.Entry{
		Time: e.now().UTC(), User: audit.SystemUser, Action: auditAction, Result: result,
		Details: map[string]string{
			"backup": j.status.Backup, "inPlace": strconv.FormatBool(j.status.InPlace),
			"started": j.status.StartedAt.UTC().Format(time.RFC3339),
		},
	}
	for k, v := range details {
		entry.Details[k] = v
	}
	if err := e.deps.Audit.Write(entry); err != nil {
		e.deps.Log.Error("could not record the restore in the audit log", "error", err)
	}
}

func (e *Engine) appliance(ctx context.Context) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	if err := e.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, fmt.Errorf("read the Appliance: %w", err)
	}
	return a, nil
}

// updateStatus applies change to the current status of the Appliance and writes it.
func (e *Engine) updateStatus(ctx context.Context, change func(*v1alpha1.ApplianceStatus)) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := e.appliance(ctx)
		if err != nil {
			return err
		}
		change(&a.Status)
		return e.deps.Client.Status().Update(ctx, a, client.FieldOwner(kube.ManagerFieldManager))
	})
	if err != nil {
		return fmt.Errorf("update the restore status: %w", err)
	}
	return nil
}

// waitFor polls ready until it reports true, and fails with errTimeout after the wait timeout. An error with less than
// a poll left counts as the timeout: the client's rate limiter refuses a request that would outlast the deadline
// before the deadline has passed.
func (e *Engine) waitFor(ctx context.Context, what string, ready func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, e.waitTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	timedOut := fmt.Errorf("wait for %s: %w", what, errTimeout)
	for {
		ok, err := ready(ctx)
		switch {
		case ctx.Err() != nil, err != nil && time.Until(deadline) < e.poll:
			return timedOut
		case err != nil:
			return err
		case ok:
			return nil
		}
		if sleep(ctx, e.poll) != nil {
			return timedOut
		}
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

// reason is a fixed description of an error, safe for a log: a timeout, the Kubernetes reason of a failed call, or
// failed. Errors of a load or a driver may carry what the log must not.
func reason(err error) string {
	if errors.Is(err, errTimeout) {
		return v1alpha1.RestoreTimeout
	}
	for _, known := range []error{ErrForeignKit, ErrTampered, ErrTruncated, ErrFormatTooNew} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	if r := apierrors.ReasonForError(err); r != "" {
		return string(r)
	}
	return v1alpha1.RestoreFailed
}
