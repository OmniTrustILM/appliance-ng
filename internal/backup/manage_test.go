package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
)

func TestRunInTheSameSecondTakesTheNextSecond(t *testing.T) {
	f := newFixture(t)
	clock := time.Date(2026, 11, 20, 2, 0, 0, 250e6, time.UTC)
	f.runner.now = func() time.Time { return clock }
	var slept []time.Duration
	f.runner.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		clock = clock.Add(d)
		return nil
	}
	first := mustRun(t, f, RunManual, nil)
	written, err := os.ReadFile(filepath.Join(f.dir, first.Name))
	if err != nil {
		t.Fatal(err)
	}
	second := mustRun(t, f, RunReplace, nil)
	if again, err := os.ReadFile(filepath.Join(f.dir, first.Name)); err != nil || !bytes.Equal(again, written) {
		t.Errorf("the first backup changed: %v", err)
	}
	// Should the name still be taken when the file is written, nothing is overwritten.
	if _, _, err := f.runner.writeLocal(context.Background(), getAppliance(t), f.key.Recipient(), first.Time.Time); !errors.Is(err, errNameTaken) {
		t.Errorf("writing over the first backup: %v", err)
	}
	if want := FileName(testHost, time.Date(2026, 11, 20, 2, 0, 1, 0, time.UTC), testVersion); second.Name != want || first.Name == second.Name {
		t.Errorf("second backup %s, want %s", second.Name, want)
	}
	if !slices.Equal(slept, []time.Duration{750 * time.Millisecond}) {
		t.Errorf("waited %v, want until the next second", slept)
	}

	f.runner.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := f.runner.Run(context.Background(), RunManual, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("run cancelled while waiting: %v", err)
	}
	if st := getAppliance(t).Status.Backup; st.Running {
		t.Error("the cancelled run shows as running")
	}
}

func TestSleepEndsWithTheContext(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep: %v", err)
	}
}

func TestStartRefusesOrRunsInTheBackground(t *testing.T) {
	f := newFixture(t)
	f.dumper.started, f.dumper.block = make(chan struct{}, 1), make(chan struct{})
	reqCtx, cancel := context.WithCancel(context.Background())
	if err := f.runner.Start(reqCtx, RunManual, nil, ByUser("admin")); err != nil {
		t.Fatal(err)
	}
	// The request ends; the backup goes on.
	cancel()
	<-f.dumper.started
	if err := f.runner.Start(context.Background(), RunManual, nil, ByUser("admin")); !errors.Is(err, ErrBusy) {
		t.Errorf("second start: %v, want ErrBusy", err)
	}
	close(f.dumper.block)
	idle(t, f.runner)
	entries := f.audit.all()
	if len(entries) != 1 || entries[0].User != "admin" || entries[0].Result != "success" {
		t.Errorf("audit %+v", entries)
	}
	if got := localNames(t, f.dir); len(got) != 1 {
		t.Errorf("backups %v", got)
	}

	f.dumper.started, f.dumper.block = nil, nil
	storeKit(t, "age1other", "issuance-2")
	if err := f.runner.Start(context.Background(), RunManual, nil, ByUser("admin")); !errors.Is(err, ErrNoKit) {
		t.Errorf("start without a usable kit: %v", err)
	}
	idle(t, f.runner)
	wantRefusalBy(t, f, "NoKit", "admin")
}

// wantRefusalBy checks that the last audit entry is a refusal of user's backup for reason.
func wantRefusalBy(t *testing.T, f *fixture, reason, user string) {
	t.Helper()
	entries := f.audit.all()
	if e := entries[len(entries)-1]; e.Details["failed"] != "refused: "+reason || e.User != user {
		t.Errorf("audit entry %+v", e)
	}
}

func TestStartLogsABackupThatFails(t *testing.T) {
	f := newFixture(t)
	f.dumper.err = errors.New(leak)
	if err := f.runner.Start(context.Background(), RunManual, nil); err != nil {
		t.Fatal(err)
	}
	idle(t, f.runner)
	if !strings.Contains(f.logs.String(), "the backup did not complete") {
		t.Errorf("log %s", f.logs)
	}
	noLeak(t, f)
}

func TestOpenOnlyListedBackups(t *testing.T) {
	f := newFixture(t)
	rec := mustRun(t, f, RunManual, nil)
	file, size, err := f.runner.Open(rec.Name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || int64(len(data)) != size || size != rec.SizeBytes {
		t.Errorf("read %d of %d bytes: %v", len(data), size, err)
	}
	local, err := f.runner.Local()
	if err != nil || len(local) != 1 || local[0].Name != rec.Name {
		t.Errorf("local %v: %v", local, err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "x"+FileSuffix+partSuffix), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../" + filepath.Base(f.dir) + "/" + rec.Name, "x" + FileSuffix + partSuffix, "/etc/passwd", ""} {
		if _, _, err := f.runner.Open(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q): %v, want ErrNotFound", name, err)
		}
	}
	f.runner.deps.Dir = filepath.Join(f.dir, rec.Name)
	if _, _, err := f.runner.Open(rec.Name); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Open in an unreadable directory: %v", err)
	}
}

func TestTestTarget(t *testing.T) {
	sftp := v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP, Retention: 14, CredentialsRef: "target-sftp",
		SFTP: &v1alpha1.SFTPTarget{Host: "sftp.example.com", Port: 22, Path: "/b", User: "u", HostKey: "SHA256:x"}}
	f := newFixture(t, sftp)
	ctx := context.Background()
	targetSecret(t, sftp, map[string][]byte{target.PasswordKey: []byte("pw")})
	store := &readableTarget{memTarget{objects: map[string][]byte{}}}
	f.runner.newTarget = func(_ v1alpha1.BackupTarget, c map[string][]byte, deps target.Deps) (target.Target, error) {
		if string(c[target.PasswordKey]) != "pw" || deps.Proxy == nil {
			t.Errorf("credentials %v, deps %+v", c, deps)
		}
		return store, nil
	}
	if err := f.runner.TestTarget(ctx, "sftp"); err != nil || len(store.objects) != 0 {
		t.Errorf("test: %v, left %v", err, store.objects)
	}
	store.putErr = fmt.Errorf("%w: %s", target.ErrAuthFailed, leak)
	if err := f.runner.TestTarget(ctx, "sftp"); target.Reason(err) != target.ReasonAuthFailed {
		t.Errorf("test: %v", err)
	}
	if err := f.runner.TestTarget(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown target: %v", err)
	}
	noLeak(t, f)

	resetRunnerCluster(t)
	if err := f.runner.TestTarget(ctx, "sftp"); err == nil {
		t.Error("tested without an Appliance")
	}
}

// readableTarget is a memTarget that reads its objects back.
type readableTarget struct{ memTarget }

func (m *readableTarget) Get(_ context.Context, name string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.objects[name])), nil
}

func TestTestTargetWithoutTargets(t *testing.T) {
	f := newFixture(t)
	if err := f.runner.TestTarget(context.Background(), "any"); !errors.Is(err, ErrNotFound) {
		t.Errorf("test: %v", err)
	}
}

func TestScanHostKeyUsesTheProxy(t *testing.T) {
	f := newFixture(t)
	f.runner.scan = func(_ context.Context, host string, port int32, deps target.Deps) (string, error) {
		if host != "sftp.example.com" || port != 2222 || deps.Proxy == nil || deps.Proxy.HTTPS != "http://proxy.example.com:3128" {
			t.Errorf("scan %s:%d with %+v", host, port, deps)
		}
		return "SHA256:abc", nil
	}
	if fp, err := f.runner.ScanHostKey(context.Background(), "sftp.example.com", 2222); err != nil || fp != "SHA256:abc" {
		t.Errorf("scan %q: %v", fp, err)
	}
	resetRunnerCluster(t)
	if _, err := f.runner.ScanHostKey(context.Background(), "sftp.example.com", 22); err == nil {
		t.Error("scanned without an Appliance")
	}
}

func TestEnteredTargetUsesTheGivenCredentials(t *testing.T) {
	f := newFixture(t)
	spec := v1alpha1.BackupTarget{Name: "restore", Type: v1alpha1.BackupTypeSFTP,
		SFTP: &v1alpha1.SFTPTarget{Host: "sftp.example.com", Port: 22, Path: "/b", User: "u", HostKey: "SHA256:x"}}
	store := &readableTarget{memTarget{objects: map[string][]byte{}}}
	f.runner.newTarget = func(got v1alpha1.BackupTarget, c map[string][]byte, deps target.Deps) (target.Target, error) {
		if got.Name != spec.Name || string(c[target.PasswordKey]) != "entered" || deps.Proxy == nil {
			t.Errorf("target %s, credentials %v, deps %+v", got.Name, c, deps)
		}
		return store, nil
	}
	tgt, err := f.runner.EnteredTarget(context.Background(), spec, map[string][]byte{target.PasswordKey: []byte("entered")})
	if err != nil || tgt != store {
		t.Errorf("entered target %v: %v", tgt, err)
	}
	resetRunnerCluster(t)
	if _, err := f.runner.EnteredTarget(context.Background(), spec, nil); err == nil {
		t.Error("opened without an Appliance")
	}
}
