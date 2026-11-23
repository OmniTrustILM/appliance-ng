package backup

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Targets name IANA time zones; the manager image has no zoneinfo of its own.

	"github.com/robfig/cron/v3"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// Defaults of a target that leaves its schedule, time zone or retention unset.
const (
	DefaultSchedule  = "0 2 * * *"
	DefaultTimeZone  = "UTC"
	DefaultRetention = 14
)

var scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseSchedule parses a five-field cron expression in the IANA time zone; empty values select the defaults.
func ParseSchedule(expr, zone string) (cron.Schedule, error) {
	expr, zone = cmp.Or(expr, DefaultSchedule), cmp.Or(zone, DefaultTimeZone)
	if zone == "Local" || strings.Contains(expr, "=") {
		return nil, errors.New("the schedule must be five cron fields and the time zone an IANA zone name")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, err
	}
	s, err := scheduleParser.Parse(expr)
	if err != nil {
		return nil, err
	}
	spec, ok := s.(*cron.SpecSchedule)
	if !ok {
		return nil, errors.New("the schedule must be five cron fields")
	}
	spec.Location = loc
	return spec, nil
}

// Overdue reports whether the target, with status st, went twice its interval without a copy: at now, two of its due
// times passed since its last success or, without one, since its first attempt.
func Overdue(t v1alpha1.BackupTarget, st *v1alpha1.TargetStatus, now time.Time) bool {
	if st == nil {
		return false
	}
	since := st.LastSuccess
	if since == nil {
		since = st.FirstAttempt
	}
	if since == nil {
		return false
	}
	s, err := ParseSchedule(t.Schedule, t.TimeZone)
	if err != nil {
		return false
	}
	second := s.Next(s.Next(since.Time))
	return !second.IsZero() && !second.After(now)
}

// entry is a cron entry: a schedule, and the time up to which its due times were handled.
type entry struct {
	schedule cron.Schedule
	handled  time.Time
}

// dueTarget is a target with its parsed schedule.
type dueTarget struct {
	name     string
	schedule cron.Schedule
}

// Scheduler takes the scheduled backups: one cron entry per distinct schedule and time zone, and at a due time one
// backup copied to every target due then.
type Scheduler struct {
	log  *slog.Logger
	now  func() time.Time
	run  func(ctx context.Context, targets []string) error
	cron *cron.Cron

	// mu guards the fields below.
	mu      sync.Mutex
	ctx     context.Context
	entries string
	jobs    []*entry
	targets []dueTarget
	// last is the due time the last scheduled backup ran for.
	last time.Time
}

// NewScheduler returns a Scheduler that takes the backups with r; now is the clock a firing entry reads.
func NewScheduler(r *Runner, now func() time.Time) *Scheduler {
	return &Scheduler{
		log: r.deps.Log,
		now: now,
		run: func(ctx context.Context, targets []string) error {
			_, err := r.Run(ctx, RunSchedule, targets)
			return err
		},
		cron: cron.New(cron.WithParser(scheduleParser), cron.WithLocation(time.UTC)),
		ctx:  context.Background(),
	}
}

// Sync schedules targets. The cron entries are rebuilt only when the set of schedules and zones changed; a target
// with a schedule that does not parse is logged and not scheduled.
func (s *Scheduler) Sync(targets []v1alpha1.BackupTarget) {
	due := make([]dueTarget, 0, len(targets))
	schedules := map[string]cron.Schedule{}
	for _, t := range targets {
		schedule, err := ParseSchedule(t.Schedule, t.TimeZone)
		if err != nil {
			s.log.Warn("backup target not scheduled: its schedule or time zone is not valid", "target", t.Name)
			continue
		}
		schedules[cmp.Or(t.TimeZone, DefaultTimeZone)+" "+cmp.Or(t.Schedule, DefaultSchedule)] = schedule
		due = append(due, dueTarget{name: t.Name, schedule: schedule})
	}
	keys := slices.Sorted(maps.Keys(schedules))
	entries := strings.Join(keys, "\n")

	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = due
	if entries == s.entries {
		return
	}
	for _, e := range s.cron.Entries() {
		s.cron.Remove(e.ID)
	}
	s.jobs = nil
	now := s.now()
	for _, key := range keys {
		job := &entry{schedule: schedules[key], handled: now}
		s.jobs = append(s.jobs, job)
		s.cron.Schedule(job.schedule, cron.FuncJob(func() { s.fire(job) }))
	}
	s.entries = entries
	s.log.Info("backup schedules set", "schedules", keys)
}

// fire takes one backup to every target due at e's latest due time, however late the job runs. Entries whose times
// coincide fire together; the first takes the backup and the others find it taken.
func (s *Scheduler) fire(e *entry) {
	now := s.now()
	s.mu.Lock()
	var at time.Time
	for t := e.schedule.Next(e.handled); !t.IsZero() && !t.After(now); t = e.schedule.Next(t) {
		at = t
	}
	if at.IsZero() {
		s.mu.Unlock()
		s.log.Info("a backup schedule fired before its due time; nothing to do")
		return
	}
	e.handled = at
	if !at.After(s.last) {
		s.mu.Unlock()
		return
	}
	var names []string
	for _, t := range s.targets {
		if t.schedule.Next(at.Add(-time.Second)).Equal(at) {
			names = append(names, t.name)
		}
	}
	if len(names) == 0 {
		s.mu.Unlock()
		s.log.Info("no backup target is due at the scheduled time", "due", at)
		return
	}
	s.last = at
	ctx := s.ctx
	s.mu.Unlock()
	s.log.Info("scheduled backup due", "due", at, "targets", names)
	// A backup that is running already takes the due targets once it ends.
	if err := s.run(ctx, names); err != nil && !errors.Is(err, ErrBusy) {
		s.log.Warn("the scheduled backup did not complete", "targets", names, "failure", Describe(err))
	}
}

// Start runs the schedule until ctx ends, then waits for a scheduled backup that runs.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	s.cron.Start()
	<-ctx.Done()
	<-s.cron.Stop().Done()
	return nil
}
