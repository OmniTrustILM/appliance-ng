package backup

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// scheduled is a Scheduler whose runs are recorded instead of taken, on a clock the test sets.
type scheduled struct {
	s    *Scheduler
	mu   sync.Mutex
	runs [][]string
	at   time.Time
	logs *bytes.Buffer
}

func newScheduled(t *testing.T) *scheduled {
	t.Helper()
	// Synced an hour before the first due time of the tests.
	sc := &scheduled{logs: &bytes.Buffer{}, at: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)}
	sc.s = NewScheduler(&Runner{deps: RunnerDeps{Log: slog.New(slog.NewJSONHandler(sc.logs, nil))}}, func() time.Time { return sc.at })
	sc.s.run = func(_ context.Context, targets []string) error {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		sc.runs = append(sc.runs, targets)
		return nil
	}
	return sc
}

// fireAll lets every cron entry fire at the instant at, as cron does when their times coincide.
func (sc *scheduled) fireAll(at time.Time) {
	sc.at = at
	for _, e := range sc.s.jobs {
		sc.s.fire(e)
	}
}

func (sc *scheduled) taken() [][]string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.Clone(sc.runs)
}

func scheduledTarget(name, schedule, zone string) v1alpha1.BackupTarget {
	return v1alpha1.BackupTarget{Name: name, Type: v1alpha1.BackupTypeNFS, Schedule: schedule, TimeZone: zone, Retention: 14,
		NFS: &v1alpha1.NFSTarget{Server: "nfs.example.com", Export: "/e"}}
}

func TestSchedulerRunsDueTargetsOnce(t *testing.T) {
	sc := newScheduled(t)
	targets := []v1alpha1.BackupTarget{
		scheduledTarget("a", "0 2 * * *", "UTC"),
		scheduledTarget("b", "0 2 * * *", "UTC"),
		// 03:00 in Prague is 02:00 UTC in winter: the same instant from another entry.
		scheduledTarget("c", "0 3 * * *", "Europe/Prague"),
		scheduledTarget("d", "30 2 * * *", "UTC"),
		scheduledTarget("defaults", "", ""),
	}
	sc.s.Sync(targets)
	entries := sc.s.cron.Entries()
	if len(entries) != 3 {
		t.Fatalf("%d cron entries, want one per distinct schedule and zone", len(entries))
	}
	sc.s.Sync(slices.Clone(targets))
	if again := sc.s.cron.Entries(); again[0].ID != entries[0].ID {
		t.Error("the same targets rebuilt the entries")
	}

	sc.fireAll(time.Date(2026, 11, 20, 2, 0, 0, 3e6, time.UTC))
	if got := sc.taken(); len(got) != 1 || !slices.Equal(got[0], []string{"a", "b", "c", "defaults"}) {
		t.Fatalf("runs %v, want one run to a, b, c and defaults", got)
	}
	sc.fireAll(time.Date(2026, 11, 20, 2, 30, 0, 0, time.UTC))
	if got := sc.taken(); len(got) != 2 || !slices.Equal(got[1], []string{"d"}) {
		t.Fatalf("runs %v, want a second run to d", got)
	}
	sc.fireAll(time.Date(2026, 11, 20, 2, 45, 0, 0, time.UTC))
	if got := sc.taken(); len(got) != 2 {
		t.Errorf("runs %v at a time no target is due", got)
	}
	if !bytes.Contains(sc.logs.Bytes(), []byte("before its due time")) {
		t.Errorf("an early firing is not logged: %s", sc.logs)
	}

	sc.s.Sync(targets[:1])
	if n := len(sc.s.cron.Entries()); n != 1 {
		t.Errorf("%d entries after removing targets, want 1", n)
	}
}

func TestSchedulerRunsALateJobForItsDueTime(t *testing.T) {
	sc := newScheduled(t)
	sc.s.Sync([]v1alpha1.BackupTarget{scheduledTarget("a", "0 2 * * *", "UTC"), scheduledTarget("b", "1 2 * * *", "UTC")})
	// The job of 02:00 runs only after 02:01, the job of 02:01 just after it.
	sc.at = time.Date(2026, 11, 20, 2, 1, 0, 2e8, time.UTC)
	sc.s.fire(sc.s.jobs[0])
	sc.s.fire(sc.s.jobs[1])
	if got := sc.taken(); len(got) != 2 || !slices.Equal(got[0], []string{"a"}) || !slices.Equal(got[1], []string{"b"}) {
		t.Errorf("runs %v, want a for 02:00 and b for 02:01", got)
	}
	// Firing again finds both due times handled.
	sc.s.fire(sc.s.jobs[0])
	if got := sc.taken(); len(got) != 2 {
		t.Errorf("runs %v after the due times were handled", got)
	}
}

func TestSchedulerLogsADueTimeWithoutTargets(t *testing.T) {
	sc := newScheduled(t)
	sc.s.Sync([]v1alpha1.BackupTarget{scheduledTarget("a", "0 2 * * *", "UTC")})
	sc.s.targets = nil
	sc.fireAll(time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC))
	if len(sc.taken()) != 0 || !bytes.Contains(sc.logs.Bytes(), []byte("no backup target is due")) {
		t.Errorf("runs %v, log %s", sc.taken(), sc.logs)
	}
}

func TestSchedulerTimeZone(t *testing.T) {
	sc := newScheduled(t)
	sc.s.Sync([]v1alpha1.BackupTarget{scheduledTarget("ny", "0 2 * * *", "America/New_York")})
	entries := sc.s.cron.Entries()
	if len(entries) != 1 {
		t.Fatalf("%d entries", len(entries))
	}
	from := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if next, want := entries[0].Schedule.Next(from), time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next run %v, want %v (02:00 in New York)", next, want)
	}
	sc.fireAll(time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC))
	if got := sc.taken(); len(got) != 0 {
		t.Errorf("ran at 02:00 UTC: %v", got)
	}
	sc.fireAll(time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC))
	if got := sc.taken(); len(got) != 1 || got[0][0] != "ny" {
		t.Errorf("runs %v, want ny at 02:00 New York time", got)
	}
}

func TestSchedulerSkipsAnInvalidSchedule(t *testing.T) {
	sc := newScheduled(t)
	sc.s.Sync([]v1alpha1.BackupTarget{
		scheduledTarget("bad", "61 2 * * *", "UTC"),
		scheduledTarget("zone", "0 2 * * *", "Mars/Olympus"),
		scheduledTarget("good", "0 2 * * *", "UTC"),
	})
	if n := len(sc.s.cron.Entries()); n != 1 {
		t.Errorf("%d entries, want the valid one", n)
	}
	if !bytes.Contains(sc.logs.Bytes(), []byte(`"target":"bad"`)) {
		t.Errorf("the invalid schedule is not logged: %s", sc.logs)
	}
}

func TestSchedulerLogsAFailedRun(t *testing.T) {
	sc := newScheduled(t)
	sc.s.run = func(context.Context, []string) error { return ErrNoKit }
	sc.s.Sync([]v1alpha1.BackupTarget{scheduledTarget("a", "", "")})
	sc.fireAll(time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC))
	if !bytes.Contains(sc.logs.Bytes(), []byte(ErrNoKit.Error())) {
		t.Errorf("log %s", sc.logs)
	}
}

func TestSchedulerStartsAndStops(t *testing.T) {
	sc := newScheduled(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sc.s.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not stop")
	}
}

func TestParseSchedule(t *testing.T) {
	for _, bad := range [][2]string{{"@daily", "UTC"}, {"0 2 * *", "UTC"}, {"0 2 * * *", "Local"}, {"0 2 * * *", "Nowhere/City"}, {"TZ=UTC 0 2 * *", "UTC"}} {
		if _, err := ParseSchedule(bad[0], bad[1]); err == nil {
			t.Errorf("%q in %q accepted", bad[0], bad[1])
		}
	}
	if _, err := ParseSchedule("*/15 1-5 * * 1-5", "Europe/Prague"); err != nil {
		t.Error(err)
	}
}

func TestOverdue(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(h time.Duration) *metav1.Time { tm := metav1.NewTime(now.Add(-h * time.Hour)); return &tm }
	daily := scheduledTarget("a", "0 2 * * *", "UTC")
	weekdays := scheduledTarget("w", "0 2 * * 1-5", "UTC")
	monday := time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	friday := metav1.NewTime(time.Date(2026, 11, 20, 2, 0, 5, 0, time.UTC))
	cases := []struct {
		name   string
		target v1alpha1.BackupTarget
		st     *v1alpha1.TargetStatus
		now    time.Time
		want   bool
	}{
		{"no status", daily, nil, now, false},
		{"never attempted", daily, &v1alpha1.TargetStatus{Name: "a"}, now, false},
		{"failed once", daily, &v1alpha1.TargetStatus{Name: "a", FirstAttempt: at(10), LastAttempt: at(10), LastError: "unreachable"}, now, false},
		{"never succeeded", daily, &v1alpha1.TargetStatus{Name: "a", FirstAttempt: at(58), LastAttempt: at(10), LastError: "unreachable"}, now, true},
		{"one day", daily, &v1alpha1.TargetStatus{Name: "a", LastSuccess: at(34)}, now, false},
		{"two days", daily, &v1alpha1.TargetStatus{Name: "a", LastSuccess: at(58)}, now, true},
		{"a weekend", weekdays, &v1alpha1.TargetStatus{Name: "w", LastSuccess: &friday}, monday, false},
		{"invalid schedule", scheduledTarget("x", "bad", "UTC"), &v1alpha1.TargetStatus{Name: "x", LastSuccess: at(500)}, now, false},
	}
	for _, c := range cases {
		if got := Overdue(c.target, c.st, c.now); got != c.want {
			t.Errorf("%s: overdue %v, want %v", c.name, got, c.want)
		}
	}
}
