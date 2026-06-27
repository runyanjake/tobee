package scheduler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes scheduler state via the abilities.Reporter contract.
//
// Done   — tick fires within the `since` window (empty if none).
// Waiting — registered ticks with their interval and next-fire estimate.
func (s *Scheduler) Reporter() abilities.Reporter { return schedReporter{s: s} }

type schedReporter struct{ s *Scheduler }

func (r schedReporter) Name() string { return "scheduler" }

func (r schedReporter) Report(_ context.Context, since time.Time) (abilities.ReportData, error) {
	var data abilities.ReportData

	// Done — recent fires in [since, now].
	type fireOut struct {
		Tick string    `json:"tick"`
		At   time.Time `json:"at"`
	}
	var done []fireOut
	for _, ev := range r.s.snapshotRecent() {
		if !since.IsZero() && ev.At.Before(since) {
			continue
		}
		done = append(done, fireOut{Tick: ev.Label, At: ev.At})
	}
	if len(done) > 0 {
		raw, err := json.Marshal(done)
		if err != nil {
			return data, err
		}
		data.Done = raw
	}

	// Waiting — registered ticks + next-fire estimate.
	type waitOut struct {
		Tick     string    `json:"tick"`
		Interval string    `json:"interval"`
		NextFire time.Time `json:"nextFire,omitempty"`
	}
	var waiting []waitOut
	for _, t := range r.s.snapshotTicks() {
		w := waitOut{Tick: t.Label, Interval: t.Interval.String()}
		if !t.LastFire.IsZero() {
			w.NextFire = t.LastFire.Add(t.Interval)
		}
		waiting = append(waiting, w)
	}
	if len(waiting) > 0 {
		raw, err := json.Marshal(waiting)
		if err != nil {
			return data, err
		}
		data.Waiting = raw
	}

	return data, nil
}
