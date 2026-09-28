// Package scheduler runs model-created jobs. The JobManager is an ingest
// source: when a job fires it emits a timer event routed back to the
// channel and user that created the job.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/ingest"
)

const firedJobRingSize = 32

// JobManager owns the set of model-scheduled jobs. Load reads persisted
// jobs and schedules them — recurring entries on a robfig cron, one-shots
// as time.AfterFunc timers. Run makes it an ingest.Source: fires are
// emitted as timer events while it runs.
//
// Concurrency: the embedded cron.Cron runs its own goroutine and dispatches
// each fire in a goroutine of its own. mu guards the per-manager maps, the
// emit function, and the recent-fires ring buffer.
type JobManager struct {
	store *JobStore
	cron  *cron.Cron

	mu      sync.Mutex
	emit    ingest.Emit         // nil while the source is not running
	entries map[string]canceler // job id → handle for cancelling its next fire
	jobs    map[string]Job      // job id → snapshot (for List / reporter)
	recent  []firedJobEvent
	head    int
	filled  bool
	loaded  bool
}

type firedJobEvent struct {
	ID      string
	Name    string
	At      time.Time
	OneShot bool
}

// canceler unifies the cancel surface for cron entries and AfterFunc timers.
type canceler interface{ cancel() }

type cronEntry struct {
	c  *cron.Cron
	id cron.EntryID
}

func (e cronEntry) cancel() { e.c.Remove(e.id) }

type timerEntry struct {
	t *time.Timer
}

func (e timerEntry) cancel() { e.t.Stop() }

// cronParser accepts the standard 5-field syntax plus robfig's @-shortcuts
// (@every 5m, @hourly, @daily, ...). We deliberately do not enable seconds.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

func NewJobManager(store *JobStore) *JobManager {
	return &JobManager{
		store:   store,
		cron:    cron.New(cron.WithParser(cronParser)),
		entries: make(map[string]canceler),
		jobs:    make(map[string]Job),
		recent:  make([]firedJobEvent, firedJobRingSize),
	}
}

// Name implements ingest.Source.
func (m *JobManager) Name() string { return "schedule" }

// Load reads persisted jobs and schedules the survivors. One-shot jobs
// whose At time has already passed are dropped from disk (misfire policy:
// skip, D-015). Idempotent.
func (m *JobManager) Load() error {
	m.mu.Lock()
	if m.loaded {
		m.mu.Unlock()
		return nil
	}
	m.loaded = true
	m.mu.Unlock()

	jobs, err := m.store.LoadAll()
	if err != nil {
		return fmt.Errorf("jobs: load: %w", err)
	}
	now := time.Now()
	loaded, dropped := 0, 0
	for _, j := range jobs {
		if !j.IsRecurring() && !j.At.IsZero() && j.At.Before(now) {
			_ = m.store.Delete(j.ID)
			dropped++
			continue
		}
		if err := m.schedule(j); err != nil {
			slog.Warn("jobs: replay schedule failed", "id", j.ID, "err", err)
			continue
		}
		loaded++
	}
	slog.Info("jobs: loaded", "loaded", loaded, "skippedMissed", dropped)
	return nil
}

// Run implements ingest.Source: it runs the cron dispatcher and emits
// fires until ctx is cancelled. A one-shot that comes due while the
// source is stopped fires into nothing and is logged.
func (m *JobManager) Run(ctx context.Context, emit ingest.Emit) error {
	if err := m.Load(); err != nil {
		return err
	}
	m.mu.Lock()
	m.emit = emit
	m.mu.Unlock()
	m.cron.Start()
	slog.Info("jobs: running")

	<-ctx.Done()

	stopCtx := m.cron.Stop()
	<-stopCtx.Done()
	m.mu.Lock()
	m.emit = nil
	m.mu.Unlock()
	slog.Info("jobs: stopped")
	return nil
}

// Create validates, persists, and schedules a new job. The caller fills in
// everything except ID/CreatedAt which Create assigns when zero.
func (m *JobManager) Create(j Job) (Job, error) {
	if err := validate(&j); err != nil {
		return Job{}, err
	}
	if j.ID == "" {
		j.ID = NewJobID()
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	if err := m.store.Save(j); err != nil {
		return Job{}, err
	}
	if err := m.schedule(j); err != nil {
		_ = m.store.Delete(j.ID)
		return Job{}, err
	}
	slog.Info("jobs: created", "id", j.ID, "name", j.Name,
		"cron", j.Cron, "at", j.At, "connector", j.Connector, "channel", j.Channel)
	return j, nil
}

// Cancel removes a scheduled job from both the in-memory schedule and disk.
// Cancelling an unknown id is not an error — the caller may have raced a
// one-shot self-cleanup.
func (m *JobManager) Cancel(id string) error {
	m.mu.Lock()
	e, ok := m.entries[id]
	if ok {
		e.cancel()
		delete(m.entries, id)
		delete(m.jobs, id)
	}
	m.mu.Unlock()
	if err := m.store.Delete(id); err != nil {
		return err
	}
	if ok {
		slog.Info("jobs: cancelled", "id", id)
	}
	return nil
}

// List returns the currently scheduled jobs in stable id order.
func (m *JobManager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	return out
}

// schedule registers j with the underlying timer (cron or AfterFunc) and
// records the handle in entries/jobs. Caller is responsible for persistence.
func (m *JobManager) schedule(j Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Replace any prior in-memory entry under the same id (rescheduling).
	if prev, ok := m.entries[j.ID]; ok {
		prev.cancel()
		delete(m.entries, j.ID)
	}

	if j.IsRecurring() {
		id, err := m.cron.AddFunc(j.Cron, func() { m.fire(j.ID) })
		if err != nil {
			return fmt.Errorf("jobs: bad cron %q: %w", j.Cron, err)
		}
		m.entries[j.ID] = cronEntry{c: m.cron, id: id}
	} else {
		d := time.Until(j.At)
		if d < 0 {
			return fmt.Errorf("jobs: one-shot time %s already passed", j.At.Format(time.RFC3339))
		}
		t := time.AfterFunc(d, func() { m.fire(j.ID) })
		m.entries[j.ID] = timerEntry{t: t}
	}
	m.jobs[j.ID] = j
	return nil
}

// fire emits the timer event, records it, and—if this was a one-shot
// —removes the job from the schedule and disk. Errors here are logged, not
// returned: the timer callback has nowhere to surface them.
func (m *JobManager) fire(id string) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	emit := m.emit
	m.mu.Unlock()
	if !ok {
		// Job was cancelled between scheduling and firing. Cron entries should
		// have been removed; a stale timer can still race in.
		return
	}

	if emit == nil {
		// The job stays on disk; the next Load applies the misfire policy.
		slog.Warn("jobs: fired while the schedule source is stopped; skipped", "id", j.ID)
		return
	}
	now := time.Now()
	emit(event.Event{
		ID:       j.ID + "@" + strconv.FormatInt(now.Unix(), 10),
		Source:   m.Name(),
		Kind:     event.KindTimer,
		Actor:    event.Actor{ID: j.User, Name: j.UserName},
		Origin:   event.Address{Connector: j.Connector, Channel: j.Channel, Thread: j.Thread},
		Content:  formatPrompt(j),
		Received: now,
	})
	slog.Info("jobs: fired", "id", j.ID, "name", j.Name, "recurring", j.IsRecurring())

	m.mu.Lock()
	m.recent[m.head] = firedJobEvent{
		ID: j.ID, Name: j.Name, At: now, OneShot: !j.IsRecurring(),
	}
	m.head = (m.head + 1) % len(m.recent)
	if m.head == 0 {
		m.filled = true
	}
	if !j.IsRecurring() {
		if entry, ok := m.entries[j.ID]; ok {
			entry.cancel()
			delete(m.entries, j.ID)
		}
		delete(m.jobs, j.ID)
	}
	m.mu.Unlock()

	if !j.IsRecurring() {
		if err := m.store.Delete(j.ID); err != nil {
			slog.Warn("jobs: post-fire cleanup failed", "id", j.ID, "err", err)
		}
	}
}

func formatPrompt(j Job) string {
	label := j.Name
	if label == "" {
		label = j.ID
	}
	return fmt.Sprintf("[scheduled fire: %s] %s", label, j.Prompt)
}

func validate(j *Job) error {
	if j.Prompt == "" {
		return fmt.Errorf("jobs: prompt is required")
	}
	if j.Connector == "" || j.Channel == "" {
		return fmt.Errorf("jobs: connector and channel are required (the fired event routes back here)")
	}
	hasCron := j.Cron != ""
	hasAt := !j.At.IsZero()
	if hasCron == hasAt {
		return fmt.Errorf(`jobs: exactly one of "cron" or "at" must be set`)
	}
	if hasCron {
		if _, err := cronParser.Parse(j.Cron); err != nil {
			return fmt.Errorf("jobs: invalid cron %q: %w", j.Cron, err)
		}
	}
	return nil
}

// snapshotRecent / snapshotJobs / nextFire support the schedules reporter.
// They live here so the lock and ring layout stay private to this file.

func (m *JobManager) snapshotRecent() []firedJobEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.head
	if m.filled {
		n = len(m.recent)
	}
	out := make([]firedJobEvent, 0, n)
	if m.filled {
		for i := 0; i < len(m.recent); i++ {
			out = append(out, m.recent[(m.head+i)%len(m.recent)])
		}
	} else {
		out = append(out, m.recent[:m.head]...)
	}
	return out
}

func (m *JobManager) snapshotJobs() []Job {
	return m.List()
}

// nextFire returns the next fire time for a job, or the zero time if the
// scheduling for it is no longer live.
func (m *JobManager) nextFire(j Job) time.Time {
	if !j.IsRecurring() {
		return j.At
	}
	m.mu.Lock()
	entry, ok := m.entries[j.ID]
	m.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	ce, ok := entry.(cronEntry)
	if !ok {
		return time.Time{}
	}
	return m.cron.Entry(ce.id).Next
}
