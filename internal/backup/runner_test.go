package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
)

const (
	testHost    = "appliance.example.com"
	testVersion = "0.1.0"
	testDump    = "-- PostgreSQL database dump\nINSERT INTO secret_table VALUES ('dump-content-never-leaves');\n"
	// leak is a string a driver or the database puts in its errors; it must not reach status, logs or the audit log.
	leak = "driver-error-with-secret-key-AKIA"
)

// fakeDumper writes data and then returns err. With block set it waits for block to close first.
type fakeDumper struct {
	data    string
	err     error
	started chan struct{}
	block   chan struct{}
	calls   int
}

func (d *fakeDumper) Dump(ctx context.Context, w io.Writer) error {
	d.calls++
	if d.started != nil {
		d.started <- struct{}{}
	}
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if _, err := io.WriteString(w, d.data); err != nil {
		return err
	}
	return d.err
}

func (d *fakeDumper) Load(context.Context, io.Reader) error { return nil }

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Write(e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAudit) all() []audit.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.entries)
}

// fixture is a Runner on the envtest cluster with an Appliance, a confirmed recovery kit and a clock that moves a
// minute per backup.
type fixture struct {
	runner *Runner
	key    *age.X25519Identity
	dir    string
	dumper *fakeDumper
	audit  *memAudit
	logs   *bytes.Buffer
}

func resetRunnerCluster(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := testClient.Delete(ctx, &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := testClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(kube.SystemNamespace)); err != nil {
		t.Fatal(err)
	}
	if err := testClient.DeleteAllOf(ctx, &otilm.Platform{}, client.InNamespace(platform.Namespace)); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, targets ...v1alpha1.BackupTarget) *fixture {
	t.Helper()
	return newHostFixture(t, testHost, targets...)
}

// newHostFixture is newFixture for the appliance hostName.
func newHostFixture(t *testing.T, hostName string, targets ...v1alpha1.BackupTarget) *fixture {
	t.Helper()
	resetRunnerCluster(t)
	ctx := context.Background()
	a := &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec: v1alpha1.ApplianceSpec{
			Node:     v1alpha1.NodeSpec{HostName: hostName},
			Proxy:    &v1alpha1.ProxySpec{HTTPS: "http://proxy.example.com:3128"},
			Platform: &v1alpha1.PlatformSpec{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}},
		},
	}
	if len(targets) > 0 {
		a.Spec.Backup = &v1alpha1.BackupSpec{Targets: targets}
	}
	if err := testClient.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	storeKit(t, key.Recipient().String(), "issuance-1")
	a.Status.RecoveryKit = &v1alpha1.RecoveryKitStatus{Recipient: key.Recipient().String(), Issuance: "issuance-1", Confirmed: true}
	a.Status.Versions = v1alpha1.Versions{Operator: "v1.1.0", ILM: "2.20.0"}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}

	f := &fixture{key: key, dir: filepath.Join(t.TempDir(), "backups"), dumper: &fakeDumper{data: testDump}, audit: &memAudit{}, logs: &bytes.Buffer{}}
	f.runner = NewRunner(RunnerDeps{
		Client:  testClient,
		Reader:  testClient,
		Dumper:  f.dumper,
		Audit:   f.audit,
		Dir:     f.dir,
		Version: testVersion,
		Targets: target.Deps{Image: "busybox"},
		Log:     slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	clock := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	f.runner.now = func() time.Time {
		clock = clock.Add(time.Minute)
		return clock
	}
	return f
}

// storeKit writes the recipient Secret the API writes when it issues a kit.
func storeKit(t *testing.T, recipient, issuance string) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: recovery.RecipientSecret},
		Data:       map[string][]byte{recovery.RecipientKey: []byte(recipient), recovery.IssuanceKey: []byte(issuance)},
	}
	err := testClient.Create(context.Background(), s)
	if apierrors.IsAlreadyExists(err) {
		err = testClient.Update(context.Background(), s)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func getAppliance(t *testing.T) *v1alpha1.Appliance {
	t.Helper()
	a := &v1alpha1.Appliance{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func backupCondition(t *testing.T) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(getAppliance(t).Status.Conditions, v1alpha1.ConditionBackupHealthy)
}

func mustRun(t *testing.T, f *fixture, reason string, targets []string, opts ...RunOption) v1alpha1.BackupRecord {
	t.Helper()
	rec, err := f.runner.Run(context.Background(), reason, targets, opts...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rec
}

func localNames(t *testing.T, dir string) []string {
	t.Helper()
	records, err := ListLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return recordNames(records)
}

// dirEntries lists every file in dir, partial ones included.
func dirEntries(t *testing.T, dir string) []string {
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

// noLeak fails when the logs, the audit entries or the status carry a driver error or dump content.
func noLeak(t *testing.T, f *fixture) {
	t.Helper()
	status := fmt.Sprintf("%+v", getAppliance(t).Status)
	entries := fmt.Sprintf("%+v", f.audit.all())
	for _, where := range []string{f.logs.String(), entries, status} {
		for _, secret := range []string{leak, "dump-content-never-leaves"} {
			if strings.Contains(where, secret) {
				t.Errorf("%q leaked into %s", secret, where)
			}
		}
	}
}

func TestRunWritesAndPrunesLocal(t *testing.T) {
	f := newFixture(t)
	f.runner.deps.KeyedConnectors = []string{"removed-provider"}
	keys := secret(t, "ilm", "removed-provider-keys", map[string][]byte{"ENCRYPTION_KEY": {0x00, 0xff, 0x80, '\n', 0x7f}})
	var names []string
	for range 4 {
		names = append(names, mustRun(t, f, RunSchedule, nil).Name)
	}
	last := mustRun(t, f, RunManual, nil, ByUser("admin"))
	names = append(names, last.Name)

	if got := localNames(t, f.dir); !slices.Equal(got, []string{names[4], names[3], names[2]}) {
		t.Fatalf("local backups %v, want the newest three of %v", got, names)
	}
	if got := dirEntries(t, f.dir); len(got) != LocalCopies {
		t.Errorf("files left %v", got)
	}
	if want := FileName(testHost, last.Time.Time, testVersion); last.Name != want || last.Time.Location() != time.UTC {
		t.Errorf("name %s, want %s", last.Name, want)
	}
	if last.Database != v1alpha1.BackupDatabaseIncluded || last.SizeBytes == 0 {
		t.Errorf("record %+v", last)
	}

	data, err := os.ReadFile(filepath.Join(f.dir, last.Name))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != last.SizeBytes {
		t.Errorf("size %d, file %d", last.SizeBytes, len(data))
	}
	m, err := Verify(bytes.NewReader(data), f.key)
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != FormatVersion || m.Appliance != testVersion || m.Operator != "v1.1.0" || m.ILM != "2.20.0" ||
		m.Database != "managed" || !m.DatabaseIncluded || m.HostName != testHost || m.ILMHostName != "ilm.example.com" ||
		!slices.Equal(m.Components, []string{"ejbca-ng-connector"}) || !m.Time.Equal(last.Time.Time) {
		t.Errorf("manifest %+v", m)
	}
	content := map[string]string{}
	err = Extract(bytes.NewReader(data), f.key, func(name string, r io.Reader) error {
		b, err := io.ReadAll(r)
		content[name] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if content["database.sql"] != testDump {
		t.Errorf("database.sql %q", content["database.sql"])
	}
	if s := content["appliance.yaml"]; !strings.Contains(s, "hostName: "+testHost) || !strings.Contains(s, "kind: Appliance") ||
		strings.Contains(s, "status") || strings.Contains(s, "resourceVersion") || strings.Contains(s, "managedFields") {
		t.Errorf("appliance.yaml:\n%s", s)
	}
	if s := content["secrets/appliance-system/"+recovery.RecipientSecret+".yaml"]; !strings.Contains(s, "kind: Secret") {
		t.Errorf("recipient Secret:\n%s", s)
	}
	var restored corev1.Secret
	if err := yaml.UnmarshalStrict([]byte(content["secrets/ilm/removed-provider-keys.yaml"]), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Type != corev1.SecretTypeOpaque || !maps.EqualFunc(restored.Data, keys.Data, bytes.Equal) {
		t.Errorf("key Secret of a removed connector: type %q, data %v, want %v", restored.Type, restored.Data, keys.Data)
	}

	st := getAppliance(t).Status.Backup
	if st == nil || st.Running || st.Last == nil || st.Last.Name != last.Name || !slices.Equal(recordNames(st.Local), localNames(t, f.dir)) {
		t.Fatalf("status %+v", st)
	}
	if c := backupCondition(t); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v", c)
	}
	entries := f.audit.all()
	if len(entries) != 5 {
		t.Fatalf("audit entries %+v", entries)
	}
	if e := entries[0]; e.User != audit.SystemUser || e.Action != "backup" || e.Result != "success" || e.Details["name"] != names[0] || e.Details["reason"] != RunSchedule {
		t.Errorf("scheduled entry %+v", e)
	}
	if e := entries[4]; e.User != "admin" || e.Details["reason"] != RunManual {
		t.Errorf("manual entry %+v", e)
	}
	noLeak(t, f)
}

func TestRunNeedsConfirmedKit(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(t *testing.T, f *fixture){
		"no kit issued": func(t *testing.T, _ *fixture) {
			if err := testClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: recovery.RecipientSecret}}); err != nil {
				t.Fatal(err)
			}
		},
		"kit not confirmed": func(t *testing.T, _ *fixture) {
			a := getAppliance(t)
			a.Status.RecoveryKit.Confirmed = false
			if err := testClient.Status().Update(ctx, a); err != nil {
				t.Fatal(err)
			}
		},
		// A replace that stored its key but never handed the kit out leaves a key nobody holds.
		"stored key not handed out": func(t *testing.T, _ *fixture) {
			other, err := recovery.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			storeKit(t, other.Recipient().String(), "issuance-2")
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			prepare(t, f)
			if _, err := f.runner.Run(ctx, RunSchedule, nil); !errors.Is(err, ErrNoKit) {
				t.Fatalf("run: %v, want ErrNoKit", err)
			}
			if f.dumper.calls != 0 {
				t.Error("the database was dumped without a kit")
			}
			if names := localNames(t, f.dir); len(names) != 0 {
				t.Errorf("backups written %v", names)
			}
			wantRefusal(t, f, "NoKit")
		})
	}
}

// wantRefusal checks the condition and the one audit entry of a refused run.
func wantRefusal(t *testing.T, f *fixture, reason string) {
	t.Helper()
	if c := backupCondition(t); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reason {
		t.Errorf("condition %+v, want %s", c, reason)
	}
	entries := f.audit.all()
	if len(entries) == 0 {
		t.Fatal("no audit entry")
	}
	if e := entries[len(entries)-1]; e.Result != "failure" || e.Details["failed"] != "refused: "+reason || e.User != audit.SystemUser {
		t.Errorf("audit entry %+v", e)
	}
	if st := getAppliance(t).Status.Backup; st != nil && st.Running {
		t.Error("a refused backup shows as running")
	}
}

func TestRunRefusesLowSpace(t *testing.T) {
	f := newFixture(t)
	var expected []uint64
	f.runner.check = func(dir string, size uint64) error {
		if dir != f.dir {
			t.Errorf("checked %s", dir)
		}
		expected = append(expected, size)
		return storage.Check(dir, size)
	}
	first := mustRun(t, f, RunSchedule, nil)
	if !slices.Equal(expected, []uint64{0}) {
		t.Errorf("first backup checked for %v bytes, want 0", expected)
	}

	f.runner.check = func(_ string, size uint64) error {
		expected = append(expected, size)
		return fmt.Errorf("%w: test", storage.ErrNoSpace)
	}
	if _, err := f.runner.Run(context.Background(), RunSchedule, nil); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("run: %v, want ErrNoSpace", err)
	}
	if expected[1] != uint64(first.SizeBytes) {
		t.Errorf("checked for %d bytes, want the last backup's %d", expected[1], first.SizeBytes)
	}
	if names := localNames(t, f.dir); !slices.Equal(names, []string{first.Name}) {
		t.Errorf("backups %v", names)
	}
	if c := backupCondition(t); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "LocalFailed" || c.Message != "noSpace" {
		t.Errorf("condition %+v", c)
	}
	wantRefusal(t, f, "LocalFailed")
	if n := len(f.audit.all()); n != 2 {
		t.Errorf("%d audit entries, want one per run", n)
	}
}

func TestRunRefusesDuringUpgrade(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	spec := getAppliance(t)
	spec.Spec.Platform.Components = nil
	p, _, err := platform.Render(spec, &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.20.0", HostName: "ilm.example.com"}}, platform.SecretNames{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := testClient.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f, RunSchedule, nil)

	p.Status.Upgrade = &otilm.UpgradeStatus{FromVersion: "2.19.0", ToVersion: "2.20.0", Phase: otilm.MigrationPhaseDraining,
		StartedAt: metav1.Now(), PhaseStartedAt: metav1.Now()}
	if err := testClient.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	// The upgrade is refused before the kit is looked at.
	a := getAppliance(t)
	a.Status.RecoveryKit = nil
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Run(ctx, RunSchedule, nil); !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("run: %v, want ErrUpgradeInProgress", err)
	}
	wantRefusal(t, f, "UpgradeInProgress")
}

// failingFile is a backup file on a disk that fills up after limit bytes.
type failingFile struct {
	*os.File
	limit int
}

func (f *failingFile) Write(p []byte) (int, error) {
	if len(p) > f.limit {
		n, _ := f.File.Write(p[:f.limit])
		f.limit = 0
		return n, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
	}
	f.limit -= len(p)
	return f.File.Write(p)
}

func TestBackupFailureKeepsPreviousCopies(t *testing.T) {
	ctx := context.Background()
	// setup takes LocalCopies backups and returns a check that a failure kept them.
	setup := func(t *testing.T) (*fixture, func(t *testing.T, message string)) {
		t.Helper()
		f := newFixture(t)
		for range LocalCopies {
			mustRun(t, f, RunSchedule, nil)
		}
		before := localNames(t, f.dir)
		lastBefore := getAppliance(t).Status.Backup.Last.Name
		return f, func(t *testing.T, message string) {
			t.Helper()
			if got := dirEntries(t, f.dir); !slices.Equal(got, slices.Sorted(slices.Values(before))) {
				t.Errorf("files %v, want the previous copies %v and no partial file", got, before)
			}
			st := getAppliance(t).Status.Backup
			if st.Running || st.Last.Name != lastBefore || !slices.Equal(recordNames(st.Local), before) {
				t.Errorf("status %+v", st)
			}
			if c := backupCondition(t); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "LocalFailed" || c.Message != message {
				t.Errorf("condition %+v, want LocalFailed %s", c, message)
			}
			if e := f.audit.all(); len(e) != LocalCopies+1 || e[len(e)-1].Result != "failure" {
				t.Errorf("audit entries %+v", e)
			}
			noLeak(t, f)
		}
	}

	t.Run("dump fails midway", func(t *testing.T) {
		f, check := setup(t)
		f.dumper.err = errors.New("pg_dump: " + leak)
		_, err := f.runner.Run(ctx, RunSchedule, nil)
		if !errors.Is(err, ErrFailed) || strings.Contains(err.Error(), leak) {
			t.Fatalf("run: %v", err)
		}
		check(t, "failed")
	})

	t.Run("disk full", func(t *testing.T) {
		f, check := setup(t)
		f.runner.create = func(path string) (localFile, error) {
			file, err := os.Create(path)
			return &failingFile{File: file, limit: 1024}, err
		}
		if _, err := f.runner.Run(ctx, RunSchedule, nil); !errors.Is(err, ErrFailed) {
			t.Fatalf("run: %v", err)
		}
		check(t, "noSpace")
	})

	t.Run("restart leaves a partial file", func(t *testing.T) {
		f, _ := setup(t)
		before := localNames(t, f.dir)
		part := filepath.Join(f.dir, FileName(testHost, time.Now(), testVersion)+partSuffix)
		writeFile(t, part, 100)
		if got := localNames(t, f.dir); !slices.Equal(got, before) {
			t.Errorf("listed %v", got)
		}
		if err := RemoveStale(f.dir); err != nil {
			t.Fatal(err)
		}
		if got := dirEntries(t, f.dir); !slices.Equal(got, slices.Sorted(slices.Values(before))) {
			t.Errorf("files %v", got)
		}
	})
}

func TestRunKeepsTheBackupWrittenWhenTheClockStepsBack(t *testing.T) {
	f := newFixture(t)
	for range LocalCopies {
		mustRun(t, f, RunSchedule, nil)
	}
	before := localNames(t, f.dir)
	f.runner.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	rec := mustRun(t, f, RunSchedule, nil)
	if got, want := localNames(t, f.dir), []string{before[0], before[1], rec.Name}; !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

func TestResetRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.runner.ResetRunning(ctx); err != nil {
		t.Fatal(err)
	}
	a := getAppliance(t)
	a.Status.Backup = &v1alpha1.BackupStatus{Running: true, Local: []v1alpha1.BackupRecord{{Name: "x" + FileSuffix, Time: metav1.Now(), Database: v1alpha1.BackupDatabaseIncluded}}}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.ResetRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if st := getAppliance(t).Status.Backup; st.Running || len(st.Local) != 1 {
		t.Errorf("status after reset %+v", st)
	}
	resetRunnerCluster(t)
	if err := f.runner.ResetRunning(ctx); err != nil {
		t.Errorf("without an Appliance: %v", err)
	}
}

// idle waits until no backup runs or waits to run.
func idle(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the runner did not become idle")
}

func TestRunOneAtATime(t *testing.T) {
	f := newFixture(t)
	f.dumper.started, f.dumper.block = make(chan struct{}, 1), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.runner.Run(context.Background(), RunSchedule, nil)
		done <- err
	}()
	<-f.dumper.started
	if !getAppliance(t).Status.Backup.Running {
		t.Error("the status does not show the running backup")
	}
	if _, err := f.runner.Run(context.Background(), RunManual, nil, ByUser("admin")); !errors.Is(err, ErrBusy) {
		t.Errorf("second run: %v, want ErrBusy", err)
	}
	// A confirmed kit's backup waits for the running one and then runs.
	if _, err := f.runner.Run(context.Background(), RunReplace, nil, ByUser("kit-admin")); !errors.Is(err, ErrBusy) {
		t.Errorf("replace run: %v, want ErrBusy", err)
	}
	close(f.dumper.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-f.dumper.started
	idle(t, f.runner)
	entries := f.audit.all()
	if len(entries) != 2 {
		t.Fatalf("audit entries %+v, want the scheduled and the waiting replace run", entries)
	}
	if e := entries[1]; e.Details["reason"] != RunReplace || e.User != "kit-admin" || e.Result != "success" {
		t.Errorf("replace entry %+v", e)
	}
	if got := localNames(t, f.dir); len(got) != 2 || getAppliance(t).Status.Backup.Running {
		t.Errorf("backups %v", got)
	}
	f.dumper.started, f.dumper.block = nil, nil
	mustRun(t, f, RunManual, nil, ByUser("admin"))
	if n := len(f.audit.all()); n != 3 {
		t.Errorf("%d audit entries, want one per run", n)
	}
}

func TestOverlappingSchedulesCopyToEveryDueTarget(t *testing.T) {
	f := newFixture(t, scheduledTarget("a", "0 2 * * *", "UTC"), scheduledTarget("b", "15 2 * * *", "UTC"),
		scheduledTarget("c", "30 2 * * *", "UTC"))
	stores := map[string]*memTarget{"a": {objects: map[string][]byte{}}, "b": {objects: map[string][]byte{}}, "c": {objects: map[string][]byte{}}}
	f.runner.newTarget = func(spec v1alpha1.BackupTarget, _ map[string][]byte, _ target.Deps) (target.Target, error) {
		return stores[spec.Name], nil
	}
	var mu sync.Mutex
	clock := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	at := func(h, m int) {
		mu.Lock()
		defer mu.Unlock()
		clock = time.Date(2026, 11, 20, h, m, 0, 0, time.UTC)
	}
	s := NewScheduler(f.runner, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	})
	s.Sync(getAppliance(t).Spec.Backup.Targets)
	f.dumper.started, f.dumper.block = make(chan struct{}, 1), make(chan struct{})

	// The backup of 02:00 still runs when b and c come due.
	at(2, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.fire(context.Background(), s.jobs[0])
	}()
	<-f.dumper.started
	at(2, 15)
	s.fire(context.Background(), s.jobs[1])
	at(2, 30)
	s.fire(context.Background(), s.jobs[2])
	close(f.dumper.block)
	<-done
	select {
	case <-f.dumper.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the targets due during the running backup got no backup")
	}
	idle(t, f.runner)

	for name, store := range stores {
		if len(store.objects) != 1 {
			t.Errorf("target %s holds %d backups, want 1", name, len(store.objects))
		}
	}
	entries := f.audit.all()
	if len(entries) != 2 || entries[1].Details["reason"] != RunSchedule || entries[1].Result != "success" {
		t.Errorf("audit entries %+v, want the backup of 02:00 and one for b and c", entries)
	}
	if strings.Contains(f.logs.String(), "the scheduled backup did not complete") {
		t.Errorf("a queued backup is logged as failed: %s", f.logs)
	}
}

func TestDescribe(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w while writing the database: failed", ErrFailed): ErrFailed.Error(),
		ErrNoKit: ErrNoKit.Error(),
		apierrors.NewNotFound(corev1.Resource("secrets"), leak): "NotFound",
		errors.New(leak): "failed",
	} {
		if got := Describe(err); got != want {
			t.Errorf("Describe(%v) = %q, want %q", err, got, want)
		}
	}
}

// memTarget keeps objects in memory; putErr and deleteErr make its operations fail.
type memTarget struct {
	objects   map[string][]byte
	putErr    error
	deleteErr error
}

func (m *memTarget) Put(_ context.Context, name string, r io.Reader, size int64) error {
	if m.putErr != nil {
		return m.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("got %d bytes, declared %d", len(data), size)
	}
	m.objects[name] = data
	return nil
}

func (m *memTarget) List(context.Context) ([]target.Object, error) {
	var out []target.Object
	for name, data := range m.objects {
		out = append(out, target.Object{Name: name, Size: int64(len(data))})
	}
	return out, nil
}

func (m *memTarget) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (m *memTarget) Delete(_ context.Context, name string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.objects, name)
	return nil
}

func TestRunCopiesToTargets(t *testing.T) {
	ctx := context.Background()
	good := v1alpha1.BackupTarget{Name: "good", Type: v1alpha1.BackupTypeS3, Retention: 2, CredentialsRef: "target-good",
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "ilm", CA: &v1alpha1.SecretKeyRef{Name: "target-good-ca", Key: "bundle.pem"}}}
	bad := v1alpha1.BackupTarget{Name: "bad", Type: v1alpha1.BackupTypeSFTP, Retention: 14, CredentialsRef: "target-bad",
		SFTP: &v1alpha1.SFTPTarget{Host: "sftp.example.com", Port: 22, Path: "/b", User: "u", HostKey: "SHA256:x"}}
	f := newFixture(t, good, bad)
	secret(t, kube.SystemNamespace, "target-good", map[string][]byte{target.AccessKeyKey: []byte("ak"), target.SecretKeyKey: []byte("sk")})
	secret(t, kube.SystemNamespace, "target-good-ca", map[string][]byte{"bundle.pem": []byte("CA PEM")})
	secret(t, kube.SystemNamespace, "target-bad", map[string][]byte{target.PasswordKey: []byte("pw")})
	secret(t, kube.SystemNamespace, "trust", map[string][]byte{"ca.crt": []byte("TRUST PEM")})
	a := getAppliance(t)
	a.Spec.Trust = &v1alpha1.SecretKeyRef{Name: "trust", Key: "ca.crt"}
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}

	older := FileName(testHost, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), testVersion)
	old := FileName(testHost, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), testVersion)
	stores := map[string]*memTarget{
		"good": {objects: map[string][]byte{older: nil, old: nil}},
		"bad":  {objects: map[string][]byte{}, putErr: fmt.Errorf("%w: %s", target.ErrAuthFailed, leak)},
	}
	creds := map[string]map[string][]byte{}
	f.runner.newTarget = func(spec v1alpha1.BackupTarget, c map[string][]byte, deps target.Deps) (target.Target, error) {
		creds[spec.Name] = c
		if deps.Proxy == nil || deps.Proxy.HTTPS != "http://proxy.example.com:3128" || string(deps.TrustPEM) != "TRUST PEM" || deps.Image != "busybox" {
			t.Errorf("deps %+v", deps)
		}
		return stores[spec.Name], nil
	}

	rec, err := f.runner.Run(ctx, RunSchedule, []string{"good", "bad", "gone"})
	if err != nil {
		t.Fatalf("a failing target fails the local backup: %v", err)
	}
	if got := slices.Sorted(maps.Keys(stores["good"].objects)); !slices.Equal(got, slices.Sorted(slices.Values([]string{rec.Name, old}))) {
		t.Errorf("good holds %v, want the newest two", got)
	}
	want := map[string][]byte{target.AccessKeyKey: []byte("ak"), target.SecretKeyKey: []byte("sk"), target.CAKey: []byte("CA PEM")}
	if !maps.EqualFunc(creds["good"], want, bytes.Equal) {
		t.Errorf("good credentials %v", creds["good"])
	}
	st := getAppliance(t).Status.Backup
	statuses := map[string]v1alpha1.TargetStatus{}
	for _, s := range st.Targets {
		statuses[s.Name] = s
	}
	if s := statuses["good"]; s.LastAttempt == nil || s.LastSuccess == nil || s.LastError != "" {
		t.Errorf("good %+v", s)
	}
	if s := statuses["bad"]; s.LastAttempt == nil || s.LastSuccess != nil || s.LastError != target.ReasonAuthFailed {
		t.Errorf("bad %+v", s)
	}
	if _, ok := statuses["gone"]; ok || len(statuses) != 2 {
		t.Errorf("statuses %+v", st.Targets)
	}
	if c := backupCondition(t); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "TargetFailed" || c.Message != "bad: authFailed" {
		t.Errorf("condition %+v", c)
	}
	if e := f.audit.all(); e[0].Result != "failure" || e[0].Details["failed"] != "bad: authFailed" {
		t.Errorf("audit entry %+v", e[0])
	}
	noLeak(t, f)

	// A failed prune fails the target; a later success clears its error.
	stores["bad"].putErr = nil
	stores["good"].deleteErr = errors.New(leak)
	mustRun(t, f, RunSchedule, []string{"good", "bad"})
	if c := backupCondition(t); c == nil || c.Message != "good: failed" {
		t.Errorf("condition after a failed prune %+v", c)
	}
	stores["good"].deleteErr = nil
	mustRun(t, f, RunSchedule, []string{"good", "bad"})
	for _, s := range getAppliance(t).Status.Backup.Targets {
		// The first attempt stays that of the first run.
		if s.LastError != "" || s.LastSuccess == nil || s.FirstAttempt == nil || !s.FirstAttempt.Before(s.LastAttempt) {
			t.Errorf("target %+v", s)
		}
	}
	if c := backupCondition(t); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v", c)
	}
	if n := len(stores["good"].objects); n != 2 {
		t.Errorf("good holds %d backups, want 2", n)
	}
	noLeak(t, f)
}

// stallTarget never finishes a Put before its context ends, as a server that stops reading does.
type stallTarget struct{ memTarget }

func (s *stallTarget) Put(ctx context.Context, _ string, _ io.Reader, _ int64) error {
	<-ctx.Done()
	return fmt.Errorf("write: %w", net.ErrClosed)
}

func TestAStalledTargetTimesOut(t *testing.T) {
	old := minCopyTimeout
	minCopyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { minCopyTimeout = old })
	f := newFixture(t, scheduledTarget("stalled", "0 2 * * *", "UTC"), scheduledTarget("good", "0 2 * * *", "UTC"))
	stores := map[string]target.Target{"stalled": &stallTarget{}, "good": &memTarget{objects: map[string][]byte{}}}
	f.runner.newTarget = func(spec v1alpha1.BackupTarget, _ map[string][]byte, _ target.Deps) (target.Target, error) {
		return stores[spec.Name], nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.runner.Run(context.Background(), RunSchedule, []string{"stalled", "good"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled target holds the run")
	}
	for _, st := range getAppliance(t).Status.Backup.Targets {
		if want := map[string]string{"stalled": target.ReasonTimeout, "good": ""}[st.Name]; st.LastError != want {
			t.Errorf("target %s: last error %q, want %q", st.Name, st.LastError, want)
		}
	}
}

func TestPruneDeletesOnlyThisAppliancesBackups(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 2, 0, 0, 0, time.UTC) }
	own := []string{FileName(testHost, day(1), testVersion), FileName(testHost, day(2), testVersion), FileName(testHost, day(3), testVersion)}
	// Written after the clock stepped back: older by its name than the backups it follows.
	written := FileName(testHost, time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), testVersion)
	others := []string{
		FileName("staging.example.com", day(1), testVersion),
		FileName(testHost+"-2", day(1), testVersion),
		testHost + "-copy.ilmbackup",
	}
	store := &memTarget{objects: map[string][]byte{written: nil}}
	for _, name := range slices.Concat(own, others) {
		store.objects[name] = nil
	}
	if err := prune(context.Background(), store, 2, testHost, written); err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(slices.Values(slices.Concat([]string{written, own[2]}, others)))
	if got := slices.Sorted(maps.Keys(store.objects)); !slices.Equal(got, want) {
		t.Errorf("target holds %v, want %v", got, want)
	}
}

func TestRunWithTheLongestHostName(t *testing.T) {
	label := strings.Repeat("a", 63)
	host := label + "." + label + "." + label + "." + strings.Repeat("b", 61)
	remote := v1alpha1.BackupTarget{Name: "remote", Type: v1alpha1.BackupTypeS3, Retention: 2, CredentialsRef: "target-remote",
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "ilm"}}
	f := newHostFixture(t, host, remote)
	secret(t, kube.SystemNamespace, "target-remote", map[string][]byte{target.AccessKeyKey: []byte("ak"), target.SecretKeyKey: []byte("sk")})
	// A backup of an earlier version, whose name has a longer version, is still this appliance's.
	older := FileName(host, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), testVersion+"-rc.10")
	other := FileName(host[:len(host)-1]+"c", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), testVersion)
	store := &memTarget{objects: map[string][]byte{older: nil, other: nil}}
	f.runner.newTarget = func(v1alpha1.BackupTarget, map[string][]byte, target.Deps) (target.Target, error) {
		return store, nil
	}

	var names []string
	for range 4 {
		names = append(names, mustRun(t, f, RunSchedule, []string{"remote"}).Name)
	}
	if got := localNames(t, f.dir); !slices.Equal(got, []string{names[3], names[2], names[1]}) {
		t.Fatalf("local backups %v, want the newest three of %v", got, names)
	}
	want := slices.Sorted(slices.Values([]string{names[3], names[2], other}))
	if got := slices.Sorted(maps.Keys(store.objects)); !slices.Equal(got, want) {
		t.Errorf("target holds %v, want %v", got, want)
	}
	if c := backupCondition(t); c.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v", c)
	}
}

func TestRunWithoutAnUnreachableDatabase(t *testing.T) {
	f := newFixture(t)
	f.dumper.data, f.dumper.err = "", ErrDatabaseUnreachable
	rec := mustRun(t, f, RunSchedule, nil)
	if rec.Database != v1alpha1.BackupDatabaseNotIncluded {
		t.Errorf("record %+v", rec)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, rec.Name))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Verify(bytes.NewReader(data), f.key)
	if err != nil {
		t.Fatal(err)
	}
	if m.DatabaseIncluded || slices.ContainsFunc(slices.Collect(maps.Keys(m.Members)), func(n string) bool { return strings.HasPrefix(n, "database.sql") }) {
		t.Errorf("manifest %+v", m)
	}
	f.dumper.data, f.dumper.err = testDump, nil
	mustRun(t, f, RunSchedule, nil)
	local := getAppliance(t).Status.Backup.Local
	if len(local) != 2 || local[0].Database != v1alpha1.BackupDatabaseIncluded || local[1].Database != v1alpha1.BackupDatabaseNotIncluded {
		t.Errorf("local %+v", local)
	}
}
