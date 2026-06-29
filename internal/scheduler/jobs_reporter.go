package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes scheduled-job state via abilities.Reporter.
func (m *JobManager) Reporter() abilities.Reporter { return jobsReporter{m: m} }

type jobsReporter struct{ m *JobManager }

func (r jobsReporter) Name() string { return "schedules" }

func (r jobsReporter) Render(_ context.Context, since time.Time) (string, string) {
	var done []firedJobEvent
	for _, ev := range r.m.snapshotRecent() {
		if ev.At.IsZero() {
			continue
		}
		if !since.IsZero() && ev.At.Before(since) {
			continue
		}
		done = append(done, ev)
	}
	jobs := r.m.snapshotJobs()
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].CreatedAt.Before(jobs[k].CreatedAt) })

	var full strings.Builder
	full.WriteString("Doing: —\n")
	if len(done) == 0 {
		full.WriteString("Done: no jobs fired in window\n")
	} else {
		full.WriteString("Done:\n")
		for _, ev := range done {
			name := ev.Name
			if name == "" {
				name = ev.ID
			}
			suffix := ""
			if ev.OneShot {
				suffix = " (one-shot)"
			}
			fmt.Fprintf(&full, "  - %s at %s%s\n", name, ev.At.UTC().Format(time.RFC3339), suffix)
		}
	}
	if len(jobs) == 0 {
		full.WriteString("Waiting: no jobs scheduled\n")
	} else {
		full.WriteString("Waiting:\n")
		for _, j := range jobs {
			name := j.Name
			if name == "" {
				name = j.ID
			}
			schedule := j.Cron
			if schedule == "" && !j.At.IsZero() {
				schedule = fmt.Sprintf("once at %s", j.At.UTC().Format(time.RFC3339))
			}
			line := fmt.Sprintf("  - %s — %s", name, schedule)
			if next := r.m.nextFire(j); !next.IsZero() {
				line += fmt.Sprintf(", next %s", next.UTC().Format(time.RFC3339))
			}
			full.WriteString(line + "\n")
		}
	}

	var summary string
	switch {
	case len(jobs) == 0 && len(done) == 0:
		summary = ""
	case len(jobs) == 0:
		summary = fmt.Sprintf("%d job%s fired in window, none still scheduled",
			len(done), schedPlural(len(done)))
	case len(done) == 0:
		summary = fmt.Sprintf("%d job%s scheduled, none fired in window",
			len(jobs), schedPlural(len(jobs)))
	default:
		summary = fmt.Sprintf("%d job%s fired in window, %d still scheduled",
			len(done), schedPlural(len(done)), len(jobs))
	}
	return full.String(), summary
}
