package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes janitor state via the abilities.Reporter contract.
func (j *Janitor) Reporter() abilities.Reporter { return janitorReporter{j: j} }

type janitorReporter struct{ j *Janitor }

func (r janitorReporter) Name() string { return "janitor" }

func (r janitorReporter) Render(_ context.Context, since time.Time) (string, string) {
	sweeps, lastEnd := r.j.snapshotSweeps()

	rotated, pruned, productive := 0, 0, 0
	var lastSweep time.Time
	for _, s := range sweeps {
		if !since.IsZero() && s.At.Before(since) {
			continue
		}
		// Empty sweeps (nothing rotated, nothing pruned) are noise.
		if s.Rotated == 0 && s.Pruned == 0 {
			continue
		}
		productive++
		rotated += s.Rotated
		pruned += s.Pruned
		if s.At.After(lastSweep) {
			lastSweep = s.At
		}
	}

	var full strings.Builder
	full.WriteString("Doing: —\n")
	if productive == 0 {
		full.WriteString("Done: no productive sweeps in window\n")
	} else {
		fmt.Fprintf(&full, "Done: %d sweep%s — rotated %d, pruned %d (last %s)\n",
			productive, schedPlural(productive), rotated, pruned, lastSweep.UTC().Format(time.RFC3339))
	}
	if lastEnd.IsZero() {
		fmt.Fprintf(&full, "Waiting: next sweep ETA unknown (interval %s)\n", r.j.sweepEvery)
	} else {
		fmt.Fprintf(&full, "Waiting: next sweep at %s (interval %s)\n",
			lastEnd.Add(r.j.sweepEvery).UTC().Format(time.RFC3339), r.j.sweepEvery)
	}

	var summary string
	if productive > 0 {
		summary = fmt.Sprintf("Janitor ran %d productive sweep%s (rotated %d, pruned %d)",
			productive, schedPlural(productive), rotated, pruned)
	}
	return full.String(), summary
}
