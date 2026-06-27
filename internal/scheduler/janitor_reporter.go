package scheduler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes janitor state via the abilities.Reporter contract.
//
// Done   — recent sweeps (rotation + archive prune counts) in the window.
// Waiting — next sweep ETA based on the last sweep and sweepEvery.
func (j *Janitor) Reporter() abilities.Reporter { return janitorReporter{j: j} }

type janitorReporter struct{ j *Janitor }

func (r janitorReporter) Name() string { return "janitor" }

func (r janitorReporter) Report(_ context.Context, since time.Time) (abilities.ReportData, error) {
	var data abilities.ReportData

	sweeps, lastEnd := r.j.snapshotSweeps()

	type sweepOut struct {
		At      time.Time `json:"at"`
		Rotated int       `json:"rotated"`
		Pruned  int       `json:"pruned"`
	}
	var done []sweepOut
	for _, s := range sweeps {
		if !since.IsZero() && s.At.Before(since) {
			continue
		}
		// Only surface non-empty sweeps unless none have happened in the
		// window — empty sweeps are usually noise.
		if s.Rotated == 0 && s.Pruned == 0 {
			continue
		}
		done = append(done, sweepOut{At: s.At, Rotated: s.Rotated, Pruned: s.Pruned})
	}
	if len(done) > 0 {
		raw, err := json.Marshal(done)
		if err != nil {
			return data, err
		}
		data.Done = raw
	}

	type waitOut struct {
		NextSweep time.Time `json:"nextSweep,omitempty"`
		Interval  string    `json:"interval"`
	}
	w := waitOut{Interval: r.j.sweepEvery.String()}
	if !lastEnd.IsZero() {
		w.NextSweep = lastEnd.Add(r.j.sweepEvery)
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return data, err
	}
	data.Waiting = raw

	return data, nil
}
