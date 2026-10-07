// Package scheduler runs model-created jobs as an ingest source; a fire routes
// back to the channel and user that created the job.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/ingest"
)

const firedJobRingSize = 32

// JobManager runs recurring jobs on robfig cron and one-shots on AfterFunc;
// fires arrive on their own goroutines, so mu guards all mutable state.
type JobManager struct {
	store *JobStore
	cron  *cron.Cron

	mu      sync.Mutex
	emit    ingest.Emit // nil while the source is not running
	entries map[string]canceler
	jobs    map[string]Job
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

// cronParser takes 5-field syntax plus @-shortcuts; seconds are deliberately off.
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

func (m *JobManager) Name() string { return "schedule" }

// Load is idempotent. Missed one-shots are deleted, not fired (D-015).
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

// Run: a one-shot that comes due while the source is stopped is logged and skipped.
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

// JobPatch is the set of fields an update may change. A nil field is left
// alone, so moving a reminder's time doesn't require restating its text.
type JobPatch struct {
	At     *time.Time
	Cron   *string
	Prompt *string
	Name   *string
}

// Update edits a pending job in place, keeping its id so the reminder the user
// already knows about stays the same reminder. Without this the model answers
// "change it to 1pm" with a second Create, leaving both to fire (D-063).
func (m *JobManager) Update(id string, p JobPatch) (Job, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return Job{}, fmt.Errorf("jobs: no reminder with id %q", id)
	}

	// Patch a copy and validate it before the live timer is touched: a bad
	// change must leave the existing reminder running rather than drop it.
	next := j
	if p.Prompt != nil {
		next.Prompt = strings.TrimSpace(*p.Prompt)
	}
	if p.Name != nil {
		next.Name = strings.TrimSpace(*p.Name)
	}
	// "at" and "cron" are mutually exclusive, so setting one clears the other.
	if p.At != nil {
		next.At, next.Cron = *p.At, ""
	}
	if p.Cron != nil {
		next.Cron, next.At = strings.TrimSpace(*p.Cron), time.Time{}
	}
	if err := validate(&next); err != nil {
		return Job{}, err
	}
	if err := m.store.Save(next); err != nil {
		return Job{}, err
	}
	// schedule replaces the existing entry and rewrites m.jobs under the lock.
	if err := m.schedule(next); err != nil {
		return Job{}, err
	}
	slog.Info("jobs: updated", "id", next.ID, "name", next.Name,
		"cron", next.Cron, "at", next.At)
	return next, nil
}

// Cancel of an unknown id is not an error: it may have raced a one-shot's self-cleanup.
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

func (m *JobManager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	return out
}

// ForUser is the pending jobs one account created, soonest first. The pinned
// reminder block is built from this, so the agent always knows what is
// outstanding without spending a call on it (D-059).
func (m *JobManager) ForUser(user string) []Job {
	if user == "" {
		return nil
	}
	var out []Job
	for _, j := range m.List() {
		if j.User == user {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool {
		a, b := m.nextFire(out[i]), m.nextFire(out[k])
		if a.IsZero() != b.IsZero() {
			return b.IsZero()
		}
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].ID < out[k].ID
	})
	return out
}

// schedule does not persist; the caller does.
func (m *JobManager) schedule(j Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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

// fire logs errors: the timer callback has nowhere to return them.
func (m *JobManager) fire(id string) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	emit := m.emit
	m.mu.Unlock()
	if !ok {
		// Cancelled, but a stale timer raced in.
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

// formatPrompt frames a fire as the note tobee left itself, now due. The old
// "[scheduled fire: …]" wording read as a status line, and the model answered
// by describing the schedule instead of saying the thing (D-053).
func formatPrompt(j Job) string {
	label := j.Name
	if label == "" {
		label = j.ID
	}
	return fmt.Sprintf("[reminder due: %s] %s", label, j.Prompt)
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

// nextFire returns the zero time once the job is no longer scheduled.
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
