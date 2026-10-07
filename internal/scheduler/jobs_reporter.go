package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

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
			fmt.Fprintf(&full, "  - %s at %s%s\n", name, FormatWhen(ev.At), suffix)
		}
	}
	if len(jobs) == 0 {
		full.WriteString("Waiting: no jobs scheduled\n")
	} else {
		full.WriteString("Waiting:\n")
		for _, j := range jobs {
			// The report is the only place pending reminders are listed, so a
			// row has to say what the reminder is for. Falling back to the id
			// left "j-3ba7ff26 — once at 4:39pm" as the whole description
			// whenever the model skipped the optional name, which is most of
			// the time (D-062).
			label := j.Name
			if label == "" {
				label = jobSummaryLabel(j.Prompt)
			}
			if label == "" {
				label = j.ID
			}
			schedule := j.Cron
			if schedule == "" && !j.At.IsZero() {
				schedule = fmt.Sprintf("once at %s", FormatWhen(j.At))
			}
			line := fmt.Sprintf("  - %s (%s) — %s", label, j.ID, schedule)
			if next := r.m.nextFire(j); !next.IsZero() {
				line += fmt.Sprintf(", next %s", FormatWhen(next))
			}
			full.WriteString(line + "\n")
		}
	}

	// Activity only. What is merely waiting is inventory: it belongs in the
	// report's "Waiting" section above, not in a summary a person reads as an
	// answer. Saying "I'm holding 2 reminders for you" here also made
	// status_summary read like the reminder-listing tool, and the model
	// started choosing it over schedule_list (D-053, D-062).
	summary := ""
	if len(done) > 0 {
		summary = fmt.Sprintf("%d reminder%s of yours went off", len(done), schedPlural(len(done)))
	}
	return full.String(), summary
}

// maxJobLabel keeps one reminder's text from dominating the report.
const maxJobLabel = 72

// jobSummaryLabel reduces a reminder's prompt to one readable line.
func jobSummaryLabel(prompt string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	if len(s) <= maxJobLabel {
		return s
	}
	return strings.TrimSpace(s[:maxJobLabel]) + "…"
}

func schedPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
