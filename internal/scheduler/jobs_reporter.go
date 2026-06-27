package scheduler

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"tobee/internal/abilities"
)

// Reporter exposes scheduled-job state via abilities.Reporter.
//
// Done    — fires within the `since` window (one-shots are noted).
// Waiting — currently scheduled jobs with their next fire time.
func (m *JobManager) Reporter() abilities.Reporter { return jobsReporter{m: m} }

type jobsReporter struct{ m *JobManager }

func (r jobsReporter) Name() string { return "schedules" }

func (r jobsReporter) Report(_ context.Context, since time.Time) (abilities.ReportData, error) {
	var data abilities.ReportData

	type fireOut struct {
		ID      string    `json:"id"`
		Name    string    `json:"name,omitempty"`
		At      time.Time `json:"at"`
		OneShot bool      `json:"oneShot,omitempty"`
	}
	var done []fireOut
	for _, ev := range r.m.snapshotRecent() {
		if ev.At.IsZero() {
			continue
		}
		if !since.IsZero() && ev.At.Before(since) {
			continue
		}
		done = append(done, fireOut{ID: ev.ID, Name: ev.Name, At: ev.At, OneShot: ev.OneShot})
	}
	if len(done) > 0 {
		raw, err := json.Marshal(done)
		if err != nil {
			return data, err
		}
		data.Done = raw
	}

	type waitOut struct {
		ID       string    `json:"id"`
		Name     string    `json:"name,omitempty"`
		Cron     string    `json:"cron,omitempty"`
		At       time.Time `json:"at,omitempty"`
		NextFire time.Time `json:"nextFire,omitempty"`
		Prompt   string    `json:"prompt,omitempty"`
	}
	jobs := r.m.snapshotJobs()
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].CreatedAt.Before(jobs[k].CreatedAt) })
	var waiting []waitOut
	for _, j := range jobs {
		waiting = append(waiting, waitOut{
			ID:       j.ID,
			Name:     j.Name,
			Cron:     j.Cron,
			At:       j.At,
			NextFire: r.m.nextFire(j),
			Prompt:   j.Prompt,
		})
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
