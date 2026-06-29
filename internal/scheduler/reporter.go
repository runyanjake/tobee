package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes scheduler state via the abilities.Reporter contract.
func (s *Scheduler) Reporter() abilities.Reporter { return schedReporter{s: s} }

type schedReporter struct{ s *Scheduler }

func (r schedReporter) Name() string { return "scheduler" }

func (r schedReporter) Render(_ context.Context, since time.Time) (string, string) {
	var done []firedEvent
	for _, ev := range r.s.snapshotRecent() {
		if !since.IsZero() && ev.At.Before(since) {
			continue
		}
		done = append(done, ev)
	}
	ticks := r.s.snapshotTicks()

	var full strings.Builder
	full.WriteString("Doing: —\n")
	if len(done) == 0 {
		full.WriteString("Done: no ticks in window\n")
	} else {
		full.WriteString("Done:\n")
		for _, ev := range done {
			fmt.Fprintf(&full, "  - %s at %s\n", ev.Label, ev.At.UTC().Format(time.RFC3339))
		}
	}
	if len(ticks) == 0 {
		full.WriteString("Waiting: no ticks registered\n")
	} else {
		full.WriteString("Waiting:\n")
		for _, t := range ticks {
			line := fmt.Sprintf("  - %s every %s", t.Label, t.Interval)
			if !t.LastFire.IsZero() {
				line += fmt.Sprintf(", next %s", t.LastFire.Add(t.Interval).UTC().Format(time.RFC3339))
			}
			full.WriteString(line + "\n")
		}
	}

	var summary string
	switch {
	case len(ticks) == 0:
		summary = ""
	case len(done) == 0:
		summary = fmt.Sprintf("Scheduler tracks %d tick%s, none fired in window",
			len(ticks), schedPlural(len(ticks)))
	default:
		summary = fmt.Sprintf("Scheduler fired %d tick%s in window across %d stream%s",
			len(done), schedPlural(len(done)), len(ticks), schedPlural(len(ticks)))
	}
	return full.String(), summary
}

// schedPlural is the package-local plural helper used by every reporter
// in this package (scheduler, jobs, janitor).
func schedPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
