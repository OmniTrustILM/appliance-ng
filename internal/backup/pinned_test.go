package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
)

const platformKind = "platform"

// runHeld takes an update backup the way the update job does: while it holds the runner.
func runHeld(t *testing.T, f *fixture, targets []string, opts ...RunOption) (v1alpha1.BackupRecord, error) {
	t.Helper()
	release, err := f.runner.Hold()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	return f.runner.Run(context.Background(), RunUpdate, targets, opts...)
}

func TestPreUpdateBackupPinned(t *testing.T) {
	f := newFixture(t)
	pinned, err := runHeld(t, f, nil, Pin(platformKind))
	if err != nil {
		t.Fatalf("update backup under a held runner: %v", err)
	}
	if pinned.Pinned != platformKind {
		t.Errorf("record %+v, want it pinned to %s", pinned, platformKind)
	}
	var names []string
	for range 5 {
		names = append(names, mustRun(t, f, RunSchedule, nil).Name)
	}
	want := []string{names[4], names[3], names[2], pinned.Name}
	if got := localNames(t, f.dir); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("local backups %v, want the newest three and the pinned one: %v", got, want)
	}
	records, err := ListLocal(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if (r.Name == pinned.Name) != (r.Pinned == platformKind) {
			t.Errorf("record %+v", r)
		}
	}
	st := getAppliance(t).Status.Backup
	if st == nil || !slices.ContainsFunc(st.Local, func(r v1alpha1.BackupRecord) bool { return r.Pinned == platformKind }) {
		t.Errorf("status %+v does not show the pinned backup", st)
	}
}

func TestUnpinOnNextUpdate(t *testing.T) {
	f := newFixture(t)
	first, err := runHeld(t, f, nil, Pin(platformKind))
	if err != nil {
		t.Fatal(err)
	}
	other, err := runHeld(t, f, nil, Pin("system"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := runHeld(t, f, nil, Pin(platformKind))
	if err != nil {
		t.Fatal(err)
	}
	var later []string
	for range 4 {
		later = append(later, mustRun(t, f, RunSchedule, nil).Name)
	}

	if err := f.runner.Unpin(platformKind, second.Name); err != nil {
		t.Fatal(err)
	}
	got := localNames(t, f.dir)
	if slices.Contains(got, first.Name) {
		t.Errorf("the older platform pin outlives the next update: %v", got)
	}
	for _, name := range []string{second.Name, other.Name} {
		if !slices.Contains(got, name) {
			t.Errorf("%s was released: %v", name, got)
		}
	}
	for _, name := range later[1:] {
		if !slices.Contains(got, name) {
			t.Errorf("newest backups lost: %v", got)
		}
	}
	if err := f.runner.Unpin(platformKind, second.Name); err != nil {
		t.Errorf("second unpin: %v", err)
	}
	if err := f.runner.Unpin("none", ""); err != nil {
		t.Errorf("unpin of a kind without a pin: %v", err)
	}
}

// TestUnpinKeepsTheNamedBackup keeps the pin of the backup the update just finished took, though a clock set back
// named it older than the pin of the update before.
func TestUnpinKeepsTheNamedBackup(t *testing.T) {
	f := newFixture(t)
	clock := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	f.runner.now = func() time.Time { return clock }
	earlier, err := runHeld(t, f, nil, Pin(platformKind))
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.AddDate(0, 0, -7)
	current, err := runHeld(t, f, nil, Pin(platformKind))
	if err != nil {
		t.Fatal(err)
	}

	if err := f.runner.Unpin(platformKind, current.Name); err != nil {
		t.Fatal(err)
	}
	records, err := ListLocal(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if (r.Name == current.Name) != (r.Pinned == platformKind) {
			t.Errorf("record %+v, want only %s pinned", r, current.Name)
		}
	}
	if !slices.Contains(localNames(t, f.dir), current.Name) {
		t.Errorf("the current backup is gone, though %s was the older pin", earlier.Name)
	}
}

func TestUpdateBackupFailsOnTargetFailure(t *testing.T) {
	good := v1alpha1.BackupTarget{Name: "good", Type: v1alpha1.BackupTypeS3, Retention: 2, CredentialsRef: "target-good",
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "ilm"}}
	bad := v1alpha1.BackupTarget{Name: "bad", Type: v1alpha1.BackupTypeS3, Retention: 2, CredentialsRef: "target-bad",
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "ilm"}}
	f := newFixture(t, good, bad)
	targetSecret(t, good, map[string][]byte{target.AccessKeyKey: []byte("ak"), target.SecretKeyKey: []byte("sk")})
	targetSecret(t, bad, map[string][]byte{target.AccessKeyKey: []byte("ak"), target.SecretKeyKey: []byte("sk")})
	stores := map[string]*memTarget{
		"good": {objects: map[string][]byte{}},
		"bad":  {objects: map[string][]byte{}, putErr: fmt.Errorf("%w: %s", target.ErrAuthFailed, leak)},
	}
	f.runner.newTarget = func(spec v1alpha1.BackupTarget, _ map[string][]byte, _ target.Deps) (target.Target, error) {
		return stores[spec.Name], nil
	}

	_, err := runHeld(t, f, []string{"good", "bad"}, Pin(platformKind))
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("update backup with a failing target: %v, want ErrFailed", err)
	}
	if got := err.Error(); !strings.Contains(got, "bad: "+target.ReasonAuthFailed) || strings.Contains(got, leak) {
		t.Errorf("error %q must name the target and its fixed reason only", got)
	}
	noLeak(t, f)

	// The scheduled run keeps tolerating a failing target.
	if _, err := f.runner.Run(context.Background(), RunSchedule, []string{"good", "bad"}); err != nil {
		t.Errorf("scheduled run with a failing target: %v", err)
	}
	stores["bad"].putErr = nil
	if _, err := runHeld(t, f, []string{"good", "bad"}, Pin(platformKind)); err != nil {
		t.Errorf("update backup to every target: %v", err)
	}
}

func TestUpdateBackupNeedsTheHeldRunner(t *testing.T) {
	f := newFixture(t)
	if _, err := f.runner.Run(context.Background(), RunUpdate, nil, Pin(platformKind)); !errors.Is(err, ErrNotHeld) {
		t.Errorf("update backup without a hold: %v, want ErrNotHeld", err)
	}
	if f.runner.Busy() {
		t.Error("a refused update backup leaves the runner busy")
	}
}

// TestUpdateBackupUnderHoldDoesNotDeadlock runs the update backup while the runner is held and refuses everything
// else meanwhile.
func TestUpdateBackupUnderHoldDoesNotDeadlock(t *testing.T) {
	f := newFixture(t)
	f.dumper.started, f.dumper.block = make(chan struct{}, 1), make(chan struct{})
	release, err := f.runner.Hold()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.runner.Run(context.Background(), RunUpdate, nil, Pin(platformKind))
		done <- err
	}()
	<-f.dumper.started
	if _, err := f.runner.Run(context.Background(), RunManual, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("manual backup meanwhile: %v, want ErrBusy", err)
	}
	if err := f.runner.Exclusive(func() error { return nil }); !errors.Is(err, ErrBusy) {
		t.Errorf("Exclusive meanwhile: %v, want ErrBusy", err)
	}
	close(f.dumper.block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the update backup deadlocks under a held runner")
	}
	if !f.runner.holding() {
		t.Error("the update backup released the hold")
	}
	release()
	if f.runner.Busy() {
		t.Error("runner busy after the release")
	}
}

func TestPinnedFilesCountTowardTheFreeSpaceCheck(t *testing.T) {
	f := newFixture(t)
	pinned, err := runHeld(t, f, nil, Pin(platformKind))
	if err != nil {
		t.Fatal(err)
	}
	newest := mustRun(t, f, RunSchedule, nil)
	for name, size := range map[string]int64{pinned.Name: 50_000, newest.Name: 10} {
		if err := os.Truncate(filepath.Join(f.dir, name), size); err != nil {
			t.Fatal(err)
		}
	}
	var expected uint64
	f.runner.check = func(_ string, size uint64) error { expected = size; return nil }
	mustRun(t, f, RunSchedule, nil)
	if expected != 50_000 {
		t.Errorf("checked for %d bytes, want the pinned backup's 50000", expected)
	}
}

// TestRunRefusedWhileAnUpdateIsUnfinished refuses every backup but the update's own while the status shows an
// unfinished update, also while no job holds the runner, as between the managers of a system update.
func TestRunRefusedWhileAnUpdateIsUnfinished(t *testing.T) {
	f := newFixture(t)
	a := getAppliance(t)
	a.Status.Update = &v1alpha1.UpdateStatus{
		Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: "talos", StartedAt: metav1.Now(),
	}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{RunSchedule, RunManual} {
		if _, err := f.runner.Run(context.Background(), reason, nil); !errors.Is(err, ErrHeld) {
			t.Errorf("%s backup during an unfinished update: %v, want ErrHeld", reason, err)
		}
	}
	if _, err := runHeld(t, f, nil, Pin(v1alpha1.UpdateKindSystem)); err != nil {
		t.Errorf("the update's own backup: %v", err)
	}
}

// TestScheduledRunKeptForTheUpdate keeps a scheduled backup an unfinished update refused, while no job holds the
// runner, and starts it once the update has ended and its job releases the runner.
func TestScheduledRunKeptForTheUpdate(t *testing.T) {
	f := newFixture(t)
	a := getAppliance(t)
	a.Status.Update = &v1alpha1.UpdateStatus{
		Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: "talos", StartedAt: metav1.Now(),
	}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	f.dumper.started, f.dumper.block = make(chan struct{}, 1), make(chan struct{})
	if _, err := f.runner.Run(context.Background(), RunSchedule, nil); !errors.Is(err, ErrHeld) {
		t.Fatalf("scheduled backup during an unfinished update: %v, want ErrHeld", err)
	}
	release, err := f.runner.Hold()
	if err != nil {
		t.Fatal(err)
	}
	a = getAppliance(t)
	a.Status.Update.Step = v1alpha1.UpdateStepDone
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.dumper.started:
		t.Fatal("the kept backup started while the update's job holds the runner")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-f.dumper.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the kept backup did not start once the update ended")
	}
	close(f.dumper.block)
	deadline := time.Now().Add(10 * time.Second)
	for f.runner.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("the kept backup did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
