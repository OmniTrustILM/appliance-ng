package update_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const preUpdate = "pre-update"

// fakeRestorer records the rollbacks it is asked to start and refuses them with err.
type fakeRestorer struct {
	mu    sync.Mutex
	calls []restoreCall
	err   error
}

type restoreCall struct {
	file string
	opts restore.Options
}

func (r *fakeRestorer) Start(_ context.Context, file string, _ age.Identity, opts restore.Options) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, restoreCall{file: file, opts: opts})
	return r.err
}

func (r *fakeRestorer) started() []restoreCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]restoreCall(nil), r.calls...)
}

// storeBundleAt writes the metadata of a loaded platform bundle, as a load stores it.
func (e *jobEnv) storeBundleAt(version, ilm, operator string) {
	e.t.Helper()
	md := bundle.Metadata{Format: 1, Kind: release.BundlePlatform, Version: version, Arch: testArch, Operator: operator, ILM: ilm, MinAppliance: "1.0.0"}
	data, err := json.Marshal(md)
	dir := filepath.Join(e.dir, release.BundlePlatform, version+"-"+testArch)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, bundle.MetadataFile), data, 0o600)
	}
	if err != nil {
		e.t.Fatal(err)
	}
}

// updated leaves the status as the platform update from from to to leaves it, finished or failed with message, with
// its backup pinned.
func (e *jobEnv) updated(from, to, message string) {
	e.t.Helper()
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Versions.PlatformBundle = to
		st.Update = &v1alpha1.UpdateStatus{
			Kind: release.BundlePlatform, From: from, To: to, Step: update.StepWait, Message: message, Backup: preUpdate,
			StartedAt: metav1.Now(), PointOfNoReturn: true,
		}
		if message == "" {
			st.Update.Step = update.StepDone
		}
	})
	e.backups.pin(preUpdate)
}

// pin keeps the local backup name pinned for a platform update.
func (b *fakeBackups) pin(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.local = append(b.local, v1alpha1.BackupRecord{Name: name, Pinned: release.BundlePlatform})
}

func rollbackKey(t *testing.T) age.Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRollbackOfTheFirstUpdateRestoresTheRelease(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.updated(fromBundle, toBundle, update.Timeout)
	restorer := &fakeRestorer{}
	jobs := e.newJobsWithRestorer(e.backups, restorer)
	if err := jobs.Rollback(t.Context(), rollbackKey(t), "admin"); err != nil {
		t.Fatalf("rollback: %v; logs:\n%s", err, e.logs)
	}
	calls := restorer.started()
	if len(calls) != 1 {
		t.Fatalf("%d restores started, want 1", len(calls))
	}
	c := calls[0]
	if c.file != filepath.Join(e.backups.dir, preUpdate) || !c.opts.InPlace || c.opts.User != "admin" || c.opts.Rollback == nil {
		t.Fatalf("restore of %s with %+v", c.file, c.opts)
	}
	r := c.opts.Rollback
	if r.Version != oldILM || r.PlatformBundle != "" {
		t.Errorf("rollback to ILM %q of bundle %q, want %s of the release", r.Version, r.PlatformBundle, oldILM)
	}
	u := r.Update
	if u.Kind != update.KindRollback || u.From != toBundle || u.To != fromBundle || u.Backup != preUpdate || u.StartedAt.IsZero() {
		t.Errorf("update status %+v", u)
	}
	e.wantHoldFree()
	if jobs.Running() {
		t.Error("the rollback holds the update jobs")
	}
	if got := e.audit.last(); got.Action != update.AuditRollbackStart || got.User != "admin" || got.Details["to"] != fromBundle {
		t.Errorf("audit %+v", got)
	}
}

func TestRollbackToTheLoadedBundle(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.storeBundleAt("1.2.0", "2.21.0", shippedOperator)
	e.updated(toBundle, "1.2.0", "")
	restorer := &fakeRestorer{}
	if err := e.newJobsWithRestorer(e.backups, restorer).Rollback(t.Context(), rollbackKey(t), "admin"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	r := restorer.started()[0].opts.Rollback
	if r.Version != newILM || r.PlatformBundle != toBundle {
		t.Errorf("rollback to ILM %q of bundle %q, want %s of %s", r.Version, r.PlatformBundle, newILM, toBundle)
	}
}

func TestRollbackRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(e *jobEnv) update.JobBackups
		err     error
		reason  string
	}{
		{"no update", func(e *jobEnv) update.JobBackups { return e.backups }, nil, update.NoPreUpdateBackup},
		{"backup not pinned", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			e.backups.local = nil
			return e.backups
		}, nil, update.NoPreUpdateBackup},
		{"rolled back already", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, "")
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.Kind = update.KindRollback })
			return e.backups
		}, nil, update.NoPreUpdateBackup},
		{"update running", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, "")
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.Step = update.StepWait })
			return e.backups
		}, nil, update.UpdateRunning},
		{"system update running", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
				st.Update = &v1alpha1.UpdateStatus{
					Kind: v1alpha1.UpdateKindSystem, From: fromBundle, To: "1.2.0", Step: update.StepTalos, StartedAt: metav1.Now(),
				}
			})
			return e.backups
		}, nil, update.UpdateRunning},
		{"previous bundle gone", func(e *jobEnv) update.JobBackups {
			e.updated("0.9.0", toBundle, update.Timeout)
			return e.backups
		}, nil, update.NoPreviousBundle},
		{"operator changed", func(e *jobEnv) update.JobBackups {
			e.storeBundleAt(toBundle, newILM, newerOperator)
			e.updated(fromBundle, toBundle, update.Timeout)
			return e.backups
		}, nil, update.OperatorChanged},
		{"upgrading", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			e.upgrading(true)
			return e.backups
		}, nil, update.UpgradeInProgress},
		{"restore running", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
				st.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepDatabase, StartedAt: metav1.Now()}
			})
			return e.backups
		}, nil, update.RestoreRunning},
		{"failed before its point of no return", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.ImagesMissing)
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.PointOfNoReturn = false })
			return e.backups
		}, nil, update.NotNeeded},
		{"external database", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			a := e.appliance()
			a.Spec.Platform.Database = &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: "appliance-database"}
			if err := e.c.Update(e.t.Context(), a); err != nil {
				e.t.Fatal(err)
			}
			return e.backups
		}, nil, update.ExternalDatabase},
		{"backup running", func(*jobEnv) update.JobBackups { return &busyBackups{} }, nil, update.BackupRunning},
		{"another operation admitted", func(*jobEnv) update.JobBackups { return &admittingBackups{} }, nil, update.Busy},
		{"foreign kit", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			return e.backups
		}, restore.ErrForeignKit, update.WrongKit},
		{"tampered backup", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			return e.backups
		}, restore.ErrTampered, update.BackupBroken},
		{"backup of another version", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			return e.backups
		}, restore.ErrVersionUnavailable{Have: oldILM, Want: newILM}, update.BackupBroken},
		{"restore admitted meanwhile", func(e *jobEnv) update.JobBackups {
			e.updated(fromBundle, toBundle, update.Timeout)
			return e.backups
		}, restore.ErrBusy, update.RestoreRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newJobEnv(t, shippedOperator)
			backups := tc.prepare(e)
			restorer := &fakeRestorer{err: tc.err}
			err := e.newJobsWithRestorer(backups, restorer).Rollback(t.Context(), rollbackKey(t), "admin")
			if update.Reason(err) != tc.reason {
				t.Fatalf("rollback: %v, want %s", err, tc.reason)
			}
			if tc.err == nil && len(restorer.started()) != 0 {
				t.Error("a refused rollback started a restore")
			}
			if u := e.appliance().Status.Update; u != nil && u.Kind == update.KindRollback && tc.name != "rolled back already" {
				t.Errorf("a refused rollback was recorded: %+v", u)
			}
			e.wantHoldFree()
		})
	}
}

func TestRollbackRefusedWhileThisManagerUpdates(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.backups.entered, e.backups.block = make(chan struct{}), make(chan struct{})
	jobs := e.newJobsWithRestorer(e.backups, &fakeRestorer{})
	e.start(jobs)
	<-e.backups.entered
	if err := jobs.Rollback(t.Context(), rollbackKey(t), "admin"); update.Reason(err) != update.UpdateRunning {
		t.Errorf("rollback while an update runs: %v", err)
	}
	close(e.backups.block)
	e.waitEnded(jobs)
}

// TestStartPlatformRefusedAfterAFailedUpdate keeps the pre-update backup of an update that failed past its point of
// no return until it is rolled back: a new update would back up the half-updated platform and release that backup.
func TestStartPlatformRefusedAfterAFailedUpdate(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.updated(fromBundle, toBundle, update.Timeout)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Versions.PlatformBundle = "" })
	jobs := e.newJobs()
	if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.LastUpdateFailed {
		t.Fatalf("start after a failed update: %v, want %s", err, update.LastUpdateFailed)
	}
	if runs, _ := e.backups.counts(); runs != 0 {
		t.Error("the refused update took a backup")
	}
	e.wantHoldFree()

	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.PointOfNoReturn = false })
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Fatalf("an update after a failure before its point of no return: %+v", u)
	}

	e.updated(fromBundle, toBundle, update.Timeout)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Versions.PlatformBundle = "" })
	e.backups.local = nil
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone {
		t.Fatalf("an update once the failed one's backup is gone: %+v", u)
	}
}

func TestStartPlatformAfterARollback(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.updated(fromBundle, toBundle, update.Timeout)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Versions.PlatformBundle = ""
		st.Update = &v1alpha1.UpdateStatus{Kind: update.KindRollback, From: toBundle, To: fromBundle, Backup: preUpdate, StartedAt: metav1.Now()}
		st.Restore = &v1alpha1.RestoreStatus{Backup: preUpdate, InPlace: true, Step: v1alpha1.RestoreStepDone, StartedAt: metav1.Now(),
			Rollback: &v1alpha1.RestoreRollback{ILMVersion: oldILM, Reinstalled: true}}
	})
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone || u.Kind != release.BundlePlatform {
		t.Fatalf("update after a rollback: %+v", u)
	}
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackHoldsTheAdmissionWhileItIsAdmitted keeps backups and loads out while the rollback is admitted.
func TestRollbackHoldsTheAdmissionWhileItIsAdmitted(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.updated(fromBundle, toBundle, update.Timeout)
	runner := backup.NewRunner(backup.RunnerDeps{Client: e.c, Reader: e.c, Audit: e.audit, Dir: e.backups.dir, Version: fromBundle})
	held := make(chan error, 1)
	restorer := &probingRestorer{probe: func() {
		_, err := runner.Hold()
		held <- err
	}}
	backups := runnerBackups{Runner: runner, fake: e.backups}
	if err := os.WriteFile(filepath.Join(e.backups.dir, "appliance.example.com-20261126T100000Z-1.0.0.ilmbackup"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.backups.dir, "appliance.example.com-20261126T100000Z-1.0.0.ilmbackup.pin"), []byte(release.BundlePlatform), 0o600); err != nil {
		t.Fatal(err)
	}
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		st.Update.Backup = "appliance.example.com-20261126T100000Z-1.0.0.ilmbackup"
	})
	if err := e.newJobsWithRestorer(backups, restorer).Rollback(t.Context(), rollbackKey(t), "admin"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	select {
	case err := <-held:
		if !errors.Is(err, backup.ErrHeld) {
			t.Errorf("a bundle load while the rollback is admitted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the restore was not started")
	}
}

// probingRestorer calls probe when the rollback's restore starts.
type probingRestorer struct{ probe func() }

func (r *probingRestorer) Start(context.Context, string, age.Identity, restore.Options) error {
	r.probe()
	return nil
}

// abandonRollback leaves the status as a rollback of the update from fromBundle to toBundle that failed at the
// uninstall and was abandoned leaves it, with the pre-update backup still pinned.
func (e *jobEnv) abandonRollback() {
	e.t.Helper()
	e.abandonRollbackAt(v1alpha1.RestoreStepUninstall)
}

// abandonRollbackAt leaves the status as a rollback abandoned after it failed at step leaves it.
func (e *jobEnv) abandonRollbackAt(step string) {
	e.t.Helper()
	e.updated(fromBundle, toBundle, update.Timeout)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
		if step != v1alpha1.RestoreStepStop {
			st.Versions.PlatformBundle = ""
		}
		st.Update = &v1alpha1.UpdateStatus{Kind: update.KindRollback, From: toBundle, To: fromBundle, Backup: preUpdate, StartedAt: metav1.Now()}
		st.Restore = &v1alpha1.RestoreStatus{Backup: preUpdate, InPlace: true, Step: v1alpha1.RestoreStepAbandoned,
			AbandonedStep: step, StartedAt: metav1.Now(), Rollback: &v1alpha1.RestoreRollback{ILMVersion: oldILM}}
	})
}

// TestStartPlatformAfterARollbackAbandonedAtStop lets an update start after a rollback abandoned before it changed
// the platform: the pre-update backup stays pinned until that update ends.
func TestStartPlatformAfterARollbackAbandonedAtStop(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.abandonRollbackAt(v1alpha1.RestoreStepStop)
	jobs := e.newJobs()
	if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); err != nil {
		t.Fatalf("start after a rollback abandoned at stop: %v", err)
	}
	if _, unpins := e.backups.counts(); unpins != 0 {
		t.Error("the start released the pinned backup")
	}
	e.waitEnded(jobs)

	e.abandonRollbackAt(v1alpha1.RestoreStepStop)
	e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Restore.Rollback.Reinstalled = true })
	if err := jobs.StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.LastUpdateFailed {
		t.Errorf("start after a rollback abandoned at the stop after its reinstall: %v, want %s", err, update.LastUpdateFailed)
	}
}

func TestStartPlatformRefusedAfterAnAbandonedRollback(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.abandonRollback()
	if err := e.newJobs().StartPlatform(t.Context(), toBundle, "admin"); update.Reason(err) != update.LastUpdateFailed {
		t.Fatalf("start after an abandoned rollback: %v, want %s", err, update.LastUpdateFailed)
	}
	if runs, _ := e.backups.counts(); runs != 0 {
		t.Error("the refused update took a backup")
	}
	e.wantHoldFree()
}

func TestRollbackAgainAfterAnAbandonedRollback(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.abandonRollback()
	restorer := &fakeRestorer{}
	if err := e.newJobsWithRestorer(e.backups, restorer).Rollback(t.Context(), rollbackKey(t), "admin"); err != nil {
		t.Fatalf("rollback after an abandoned one: %v", err)
	}
	r := restorer.started()[0].opts.Rollback
	u := r.Update
	if r.Version != oldILM || r.PlatformBundle != "" || u.Kind != update.KindRollback || u.From != toBundle || u.To != fromBundle || u.Backup != preUpdate {
		t.Errorf("rollback %+v, want again from %s to %s with the pre-update backup", r, toBundle, fromBundle)
	}
}

// TestRollbackRefusedTellsWhy tells why the last platform update cannot be rolled back, without starting anything,
// and nothing once it can.
func TestRollbackRefusedTellsWhy(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	restorer := &fakeRestorer{}
	jobs := e.newJobsWithRestorer(e.backups, restorer)
	if got := jobs.RollbackRefused(t.Context()); got != update.NoPreUpdateBackup {
		t.Errorf("RollbackRefused without an update = %q, want %s", got, update.NoPreUpdateBackup)
	}
	e.updated(fromBundle, toBundle, "")
	if got := jobs.RollbackRefused(t.Context()); got != "" {
		t.Errorf("RollbackRefused after an update = %q, want none", got)
	}
	e.storeBundleAt(toBundle, newILM, newerOperator)
	if got := jobs.RollbackRefused(t.Context()); got != update.OperatorChanged {
		t.Errorf("RollbackRefused after an operator update = %q, want %s", got, update.OperatorChanged)
	}
	if len(restorer.started()) != 0 {
		t.Error("RollbackRefused started a restore")
	}
	e.wantHoldFree()
}

// TestFailedUpdateBeforeItsPointOfNoReturnKeepsThePreviousBundle puts the platform bundle that ran before back when
// an update fails before its point of no return, so that a retried update and its rollback start from it.
func TestFailedUpdateBeforeItsPointOfNoReturnKeepsThePreviousBundle(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	e.crashedAt(update.StepVersion, 3)
	e.upgrading(true)
	jobs := e.newJobs()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(jobs); u.Message != update.PlatformUpgrading || u.PointOfNoReturn {
		t.Fatalf("update %+v, want failed before its point of no return", u)
	}
	if got := e.appliance().Status.Versions.PlatformBundle; got != "" {
		t.Fatalf("platform bundle %q after the failed update, want the release's own", got)
	}
	e.upgrading(false)
	e.start(jobs)
	if u := e.waitEnded(jobs); u.Step != update.StepDone || u.From != fromBundle || u.FromBundle != "" {
		t.Fatalf("retried update %+v, want from %s", u, fromBundle)
	}
	e.backups.pin(preUpdate)
	restorer := &fakeRestorer{}
	if err := e.newJobsWithRestorer(e.backups, restorer).Rollback(t.Context(), rollbackKey(t), "admin"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if r := restorer.started()[0].opts.Rollback; r.Version != oldILM || r.PlatformBundle != "" {
		t.Errorf("rollback to ILM %q of bundle %q, want %s of the release", r.Version, r.PlatformBundle, oldILM)
	}
}

// TestFailedUpdateKeepsTheBundleWhoseOperatorRuns keeps the platform bundle of a failed update whose newer ILM
// operator the add-on pass applied: going back to the previous one would downgrade the operator.
func TestFailedUpdateKeepsTheBundleWhoseOperatorRuns(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	e.crashedAt(update.StepVersion, 3)
	e.upgrading(true)
	jobs := e.newJobs()
	if err := jobs.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := e.waitEnded(jobs); u.Message != update.PlatformUpgrading {
		t.Fatalf("update %+v", u)
	}
	if got := e.appliance().Status.Versions.PlatformBundle; got != toBundle {
		t.Errorf("platform bundle %q, want %s whose operator runs", got, toBundle)
	}
}

// TestRollbackGoesBackToTheILMTheUpdateStartedFrom rolls a platform update back to the ILM version it started from,
// also after a system update whose release embeds another ILM: from a loaded platform bundle carrying it, else from
// the release only when it embeds that version, else not at all.
func TestRollbackGoesBackToTheILMTheUpdateStartedFrom(t *testing.T) {
	const systemILM = "2.19.5"
	cases := []struct {
		name       string
		releaseILM string
		loaded     bool
		version    string
		bundle     string
		reason     string
	}{
		{"release embeds another ILM", systemILM, false, "", "", update.NoPreviousBundle},
		{"a loaded bundle carries it", systemILM, true, oldILM, "1.0.5", ""},
		{"the release embeds it", oldILM, false, oldILM, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newJobEnv(t, shippedOperator)
			e.releaseILM = tc.releaseILM
			if tc.loaded {
				e.storeBundleAt("1.0.5", oldILM, shippedOperator)
				e.updateStatus(func(st *v1alpha1.ApplianceStatus) {
					st.Updates.Loaded = append(st.Updates.Loaded, v1alpha1.BundleInfo{
						Kind: release.BundlePlatform, Version: "1.0.5", Arch: testArch, ILM: oldILM, Operator: shippedOperator,
						Blocked: update.OlderThanRunning,
					})
				})
			}
			e.updated(fromBundle, toBundle, update.Timeout)
			e.updateStatus(func(st *v1alpha1.ApplianceStatus) { st.Update.FromILM = oldILM })
			restorer := &fakeRestorer{}
			err := e.newJobsWithRestorer(e.backups, restorer).Rollback(t.Context(), rollbackKey(t), "admin")
			if tc.reason != "" {
				if update.Reason(err) != tc.reason || len(restorer.started()) != 0 {
					t.Fatalf("rollback: %v, want %s", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			r := restorer.started()[0].opts.Rollback
			if r.Version != tc.version || r.PlatformBundle != tc.bundle || r.Update.FromILM != oldILM {
				t.Errorf("rollback to ILM %q of bundle %q (%+v), want %s of %q", r.Version, r.PlatformBundle, r.Update, tc.version, tc.bundle)
			}
		})
	}
}

// TestPlatformUpdateRecordsTheILMItStartsFrom records the ILM version that runs when a platform update starts.
func TestPlatformUpdateRecordsTheILMItStartsFrom(t *testing.T) {
	e := newJobEnv(t, newerOperator)
	jobs := e.newJobs()
	e.start(jobs)
	if u := e.waitEnded(jobs); u.FromILM != oldILM {
		t.Errorf("update %+v, want it to record ILM %s", u, oldILM)
	}
}
