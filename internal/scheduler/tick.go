// Package scheduler publishes synthetic Envelopes onto the agent bus on a
// fixed interval. Day one it is idle — no ticks are registered. The hook
// exists so reflection / idle-check / proactive pings can be wired later
// without reshaping the loop.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"tobee/internal/integrations"
)

// Scheduler publishes ticks as Envelopes on the agent bus.
type Scheduler struct {
	bus   *integrations.Bus
	ticks []tick
}

type tick struct {
	interval time.Duration
	env      integrations.Envelope
}

func New(bus *integrations.Bus) *Scheduler {
	return &Scheduler{bus: bus}
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
	s.ticks = append(s.ticks, tick{interval: interval, env: env})
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

func (s *Scheduler) run(ctx context.Context, t tick) {
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
		}
	}
}
