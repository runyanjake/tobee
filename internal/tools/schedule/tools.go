// Package schedule wires the JobManager into the tool registry so the model
// can set timers and recurring jobs for itself. When a scheduled job fires,
// it lands on the agent bus as a synthetic Envelope routed back to the
// channel and user that created it.
package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"tobee/internal/scheduler"
	"tobee/internal/scope"
	"tobee/internal/tools"
)

// Register adds schedule.* tools backed by m.
func Register(reg *tools.Registry, m *scheduler.JobManager) {
	reg.MustRegister(tools.Spec{
		Name: "schedule.create",
		Description: `Schedule a future prompt to yourself. Exactly one of "at" or "cron" must be set.

- at:   RFC3339 timestamp ("2026-06-26T18:30:00-07:00") OR relative ("in 10m", "in 2h30m") for a one-shot.
- cron: standard 5-field cron expression ("0 9 * * MON-FRI") OR robfig descriptor ("@every 30m", "@hourly", "@daily").

When the schedule fires, the "prompt" text is delivered as the next user-message-equivalent on the same channel that created the job, prefixed with "[scheduled fire: <name>]". Use this for reminders, follow-ups, and periodic checks. Returns the job id — keep it if you may want to cancel.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "What to say to yourself when the timer fires. Write it as a directive (\"check the deploy status and report back\")."},
				"at":     {"type": "string", "description": "RFC3339 absolute time or \"in <duration>\". Mutually exclusive with cron."},
				"cron":   {"type": "string", "description": "Cron expression for recurring fires. Mutually exclusive with at."},
				"name":   {"type": "string", "description": "Optional short label used in logs and the fire marker."}
			},
			"required": ["prompt"]
		}`),
		Handler: createHandler(m),
	})

	reg.MustRegister(tools.Spec{
		Name:        "schedule.cancel",
		Description: `Cancel a scheduled job by id. Cancelling an unknown or already-fired id is not an error.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Job id returned by schedule.create"}
			},
			"required": ["id"]
		}`),
		Handler: cancelHandler(m),
	})

	reg.MustRegister(tools.Spec{
		Name:        "schedule.list",
		Description: `List currently scheduled jobs. Returns "<id>  <when>  <name>  <prompt>" rows.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {}
		}`),
		Handler: listHandler(m),
	})
}

func createHandler(m *scheduler.JobManager) tools.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Prompt string `json:"prompt"`
			At     string `json:"at"`
			Cron   string `json:"cron"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		s, ok := scope.From(ctx)
		if !ok || s.Integration == "" || s.Channel == "" {
			return "", fmt.Errorf(`schedule.create unavailable: no originating channel attached to this turn`)
		}

		j := scheduler.Job{
			Name:        strings.TrimSpace(in.Name),
			Prompt:      in.Prompt,
			Cron:        strings.TrimSpace(in.Cron),
			Integration: s.Integration,
			Channel:     s.Channel,
			Thread:      s.Thread,
			User:        s.User,
			UserName:    s.UserName,
		}
		if strings.TrimSpace(in.At) != "" {
			t, err := parseAt(in.At)
			if err != nil {
				return "", err
			}
			j.At = t
		}

		created, err := m.Create(j)
		if err != nil {
			return "", err
		}
		when := ""
		if created.IsRecurring() {
			when = "cron=" + created.Cron
		} else {
			when = "at=" + created.At.Format(time.RFC3339)
		}
		return fmt.Sprintf("scheduled %s (%s)", created.ID, when), nil
	}
}

func cancelHandler(m *scheduler.JobManager) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if in.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		if err := m.Cancel(in.ID); err != nil {
			return "", err
		}
		return "cancelled " + in.ID, nil
	}
}

func listHandler(m *scheduler.JobManager) tools.Handler {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		jobs := m.List()
		if len(jobs) == 0 {
			return "(no scheduled jobs)", nil
		}
		sort.Slice(jobs, func(i, k int) bool { return jobs[i].CreatedAt.Before(jobs[k].CreatedAt) })
		var sb strings.Builder
		for _, j := range jobs {
			when := j.Cron
			if when == "" {
				when = "at " + j.At.Format(time.RFC3339)
			}
			label := j.Name
			if label == "" {
				label = "-"
			}
			fmt.Fprintf(&sb, "%s  %s  %s  %s\n", j.ID, when, label, j.Prompt)
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}
}

// parseAt accepts either RFC3339 or "in <duration>" (e.g. "in 10m").
func parseAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "in "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid duration %q: %w", rest, err)
		}
		return time.Now().Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q (expected RFC3339 or \"in <duration>\"): %w", s, err)
	}
	return t, nil
}
