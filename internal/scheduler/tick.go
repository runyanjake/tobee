// Package scheduler publishes synthetic Envelopes onto the agent bus on a
// fixed interval. Day one it is idle — no ticks are registered. The hook
// exists so reflection / idle-check / proactive pings can be wired later
// without reshaping the loop.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/runyanjake/tobee/internal/integrations"
)

// recentRingSize bounds the in-memory log of recent fires used by the
// status reporter. Old entries are overwritten in place.
const recentRingSize = 16

// Scheduler publishes ticks as Envelopes on the agent bus.
type Scheduler struct {
	bus   *integrations.Bus
	ticks []*tick

	mu     sync.RWMutex
	recent []firedEvent // ring buffer; head is at recentHead
	head   int
	filled bool
}

type tick struct {
	label    string
	interval time.Duration
	env      integrations.Envelope

	mu       sync.RWMutex
	lastFire time.Time
}

type firedEvent struct {
	Label string
	At    time.Time
}

func New(bus *integrations.Bus) *Scheduler {
	return &Scheduler{
		bus:    bus,
		recent: make([]firedEvent, recentRingSize),
	}
}

// Every registers a tick to publish env at the given interval.
// Must be called before Start.
func (s *Scheduler) Every(interval time.Duration, env integrations.Envelope) {
	if env.Integration == "" {
		env.Integration = "scheduler"
	}
	if env.Channel == "" {
		env.Channel = "tick"
	}
	s.ticks = append(s.ticks, &tick{
		label:    env.Integration + ":" + env.Channel,
		interval: interval,
		env:      env,
	})
}

// Start launches a goroutine per registered tick. Cancel ctx to stop all.
func (s *Scheduler) Start(ctx context.Context) {
	if len(s.ticks) == 0 {
		slog.Info("scheduler: no ticks registered")
		return
	}
	for _, t := range s.ticks {
		go s.run(ctx, t)
	}
}

func (s *Scheduler) run(ctx context.Context, t *tick) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			env := t.env
			env.Received = now
			s.bus.Publish(env)
			t.mu.Lock()
			t.lastFire = now
			t.mu.Unlock()
			s.recordFire(t.label, now)
		}
	}
}

func (s *Scheduler) recordFire(label string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent[s.head] = firedEvent{Label: label, At: at}
	s.head = (s.head + 1) % len(s.recent)
	if s.head == 0 {
		s.filled = true
	}
}

// snapshotRecent returns the recent fires in chronological order, oldest
// first. Used by the scheduler's Reporter.
func (s *Scheduler) snapshotRecent() []firedEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := s.head
	if s.filled {
		n = len(s.recent)
	}
	out := make([]firedEvent, 0, n)
	if s.filled {
		for i := 0; i < len(s.recent); i++ {
			out = append(out, s.recent[(s.head+i)%len(s.recent)])
		}
	} else {
		out = append(out, s.recent[:s.head]...)
	}
	return out
}

// snapshotTicks returns label/interval/lastFire for each registered tick.
func (s *Scheduler) snapshotTicks() []tickSummary {
	out := make([]tickSummary, 0, len(s.ticks))
	for _, t := range s.ticks {
		t.mu.RLock()
		last := t.lastFire
		t.mu.RUnlock()
		out = append(out, tickSummary{
			Label:    t.label,
			Interval: t.interval,
			LastFire: last,
		})
	}
	return out
}

type tickSummary struct {
	Label    string
	Interval time.Duration
	LastFire time.Time
}
