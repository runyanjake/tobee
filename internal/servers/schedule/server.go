// Package schedule is the built-in "schedule" MCP server for model-created timers and
// recurring jobs; a fired job replies to the channel and user that created it.
package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/scheduler"
	"github.com/runyanjake/tobee/internal/scope"
)

// The pinned block changes whenever a reminder is set or fires, so it sits
// below the memory blocks and well below the static prompt (D-017).
const (
	pendingURI      = "schedule://pending"
	pendingPriority = 0.4
	pendingMax      = 10
)

func New(instructions string, m *scheduler.JobManager) *mcpserver.Server {
	srv := mcpserver.New("schedule", instructions)

	// What is outstanding is context, not something to spend a call
	// discovering. Derived from the job store on every read, so there is no
	// second copy to keep in step (D-059).
	srv.AddResource(mcpserver.Resource{
		URI:         pendingURI,
		Name:        "pending",
		Description: "The reminders and jobs this user has waiting.",
		MIMEType:    "text/markdown",
		Pinned:      true,
		PinPriority: pendingPriority,
		Read:        readPending(m),
	})
	srv.Add(mcpserver.Tool{
		Name: "create",
		Description: `Schedule a future prompt to yourself. Exactly one of "at" or "cron" must be set.

- at:   RFC3339 timestamp ("2026-06-26T18:30:00-07:00") OR relative ("in 10m", "in 2h30m") for a one-shot.
        A clock time the user names ("4:40pm") is local: build it from the now and tz stamped in <context>,
        and prefer an absolute timestamp with that offset over a duration you worked out in your head.
- cron: standard 5-field cron expression ("0 9 * * MON-FRI") OR robfig descriptor ("@every 30m", "@hourly", "@daily").

When the time arrives the "prompt" text comes back to you as a new message on the same channel, prefixed with "[reminder due: <name>]" — at that point you say it to the user, you don't describe the schedule. Use this for reminders, follow-ups, and periodic checks. Returns the job id — keep it if you may want to change or cancel it.

Only for something new. To move, reword or reschedule a reminder that already exists, call schedule_update with its id: a second create leaves both to fire.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": {"type": "string", "description": "The note that comes back to you when the time arrives. For a reminder, write what the user needs to hear (\"leave for basketball with Damian\"); for a task, write the directive (\"check the deploy status and report back\"). Not a confirmation — you are writing to your future self, not to the user."},
				"at":     {"type": "string", "description": "RFC3339 absolute time or \"in <duration>\". Mutually exclusive with cron."},
				"cron":   {"type": "string", "description": "Cron expression for recurring fires. Mutually exclusive with at."},
				"name":   {"type": "string", "description": "Optional short label used in logs and the fire marker."}
			},
			"required": ["prompt"]
		}`),
		Handler: createHandler(m),
	})

	srv.Add(mcpserver.Tool{
		Name: "update",
		Description: `Change a reminder that already exists, keeping its id. This is what to call when the user moves, rewords or reschedules something they already asked for — not schedule_create, which would leave the original to fire as well.

Only the fields you pass change; the rest stay as they are. "at" and "cron" replace one another, so setting one clears the other.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id":     {"type": "string", "description": "Job id, from schedule_create or schedule_list"},
				"at":     {"type": "string", "description": "New fire time: RFC3339 absolute time or \"in <duration>\". Clears cron."},
				"cron":   {"type": "string", "description": "New cron expression. Clears at."},
				"prompt": {"type": "string", "description": "New note to come back to you when it fires. Rewrite this when the old text no longer says what the user needs to hear."},
				"name":   {"type": "string", "description": "New short label."}
			},
			"required": ["id"]
		}`),
		Handler: updateHandler(m),
	})

	srv.Add(mcpserver.Tool{
		Name:        "cancel",
		Description: `Cancel a scheduled job by id. Cancelling an unknown or already-fired id is not an error.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "Job id returned by schedule_create"}
			},
			"required": ["id"]
		}`),
		Handler: cancelHandler(m),
	})

	srv.Add(mcpserver.Tool{
		Name: "list",
		Description: `The reminders and recurring jobs set right now, as "<id>  <when>  <name>  <prompt>" rows. ` +
			`This is what to call whenever the user asks what reminders or schedules they have.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {}
		}`),
		ReadOnly: true,
		Handler:  listHandler(m),
	})
	return srv
}

func createHandler(m *scheduler.JobManager) mcpserver.Handler {
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
		if !ok || s.Connector == "" || s.Channel == "" {
			return "", fmt.Errorf(`schedule_create unavailable: no originating channel attached to this turn`)
		}

		j := scheduler.Job{
			Name:      strings.TrimSpace(in.Name),
			Prompt:    in.Prompt,
			Cron:      strings.TrimSpace(in.Cron),
			Connector: s.Connector,
			Channel:   s.Channel,
			Thread:    s.Thread,
			User:      s.User,
			UserName:  s.UserName,
		}
		if strings.TrimSpace(in.At) != "" {
			t, err := parseAt(in.At)
			if err != nil {
				return "", err
			}
			j.At = t
		}

		// A second reminder for something already scheduled is nearly always an
		// edit the model expressed as a create, which leaves both to fire.
		// Refuse it here and name the id, the way a duplicate memory write is
		// refused (D-054, D-063).
		if s.User != "" {
			if clash, ok := duplicatePending(m.ForUser(s.User), j.Prompt); ok {
				when := clash.Cron
				if when == "" {
					when = scheduler.FormatWhen(clash.At)
				}
				return "", fmt.Errorf("%s already covers that (%s, %q). Change that one with schedule_update, or cancel it first — a second reminder would fire as well",
					clash.ID, when, clash.Prompt)
			}
		}

		created, err := m.Create(j)
		if err != nil {
			return "", err
		}
		// This line is what the user sees under the reply (D-047), so it says
		// the fire time the way a clock does, in the local zone.
		if created.IsRecurring() {
			return fmt.Sprintf("scheduled %s (cron %s)", created.ID, created.Cron), nil
		}
		return fmt.Sprintf("scheduled %s (fires %s, %s)", created.ID,
			scheduler.FormatWhen(created.At), created.At.Format(time.RFC3339)), nil
	}
}

func updateHandler(m *scheduler.JobManager) mcpserver.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		// Pointers, so "not passed" is distinguishable from "set to empty":
		// moving a time must not blank the prompt.
		var in struct {
			ID     string  `json:"id"`
			At     *string `json:"at"`
			Cron   *string `json:"cron"`
			Prompt *string `json:"prompt"`
			Name   *string `json:"name"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		id := strings.TrimSpace(in.ID)
		if id == "" {
			return "", fmt.Errorf("id is required (schedule_list gives the ids)")
		}
		hasAt := in.At != nil && strings.TrimSpace(*in.At) != ""
		hasCron := in.Cron != nil && strings.TrimSpace(*in.Cron) != ""
		if hasAt && hasCron {
			return "", fmt.Errorf(`pass "at" or "cron", not both: a reminder is either one-shot or recurring`)
		}

		var p scheduler.JobPatch
		if hasAt {
			t, err := parseAt(*in.At)
			if err != nil {
				return "", err
			}
			p.At = &t
		}
		if hasCron {
			p.Cron = in.Cron
		}
		if in.Prompt != nil {
			p.Prompt = in.Prompt
		}
		if in.Name != nil {
			p.Name = in.Name
		}
		if p.At == nil && p.Cron == nil && p.Prompt == nil && p.Name == nil {
			return "", fmt.Errorf("nothing to change: pass at, cron, prompt or name")
		}

		updated, err := m.Update(id, p)
		if err != nil {
			return "", err
		}
		if updated.IsRecurring() {
			return fmt.Sprintf("updated %s (cron %s)", updated.ID, updated.Cron), nil
		}
		return fmt.Sprintf("updated %s (now fires %s, %s)", updated.ID,
			scheduler.FormatWhen(updated.At), updated.At.Format(time.RFC3339)), nil
	}
}

// minDuplicatePrompt keeps the containment test off very short prompts, where
// one reminder's words sit inside an unrelated one by coincidence.
const minDuplicatePrompt = 10

// duplicatePending finds a pending job that is plainly the same reminder. The
// test is deliberately narrow — equal once case, punctuation and spacing are
// ignored, or one wholly contained in the other — because refusing a genuinely
// new reminder costs more than letting a near-duplicate through. A refusal is
// per argument set, so a reworded prompt still goes through (D-063).
func duplicatePending(pending []scheduler.Job, prompt string) (scheduler.Job, bool) {
	want := normalizePrompt(prompt)
	if len(want) < minDuplicatePrompt {
		return scheduler.Job{}, false
	}
	for _, j := range pending {
		got := normalizePrompt(j.Prompt)
		if len(got) < minDuplicatePrompt {
			continue
		}
		if got == want || strings.Contains(got, want) || strings.Contains(want, got) {
			return j, true
		}
	}
	return scheduler.Job{}, false
}

// normalizePrompt reduces a reminder to its words: lowercase, letters and
// digits only, single-spaced.
func normalizePrompt(s string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if gap && b.Len() > 0 {
				b.WriteByte(' ')
			}
			gap = false
			b.WriteRune(r)
			continue
		}
		gap = true
	}
	return b.String()
}

func cancelHandler(m *scheduler.JobManager) mcpserver.Handler {
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

func listHandler(m *scheduler.JobManager) mcpserver.Handler {
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
				when = scheduler.FormatWhen(j.At)
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

// parseAt accepts RFC3339 or "in <duration>" (e.g. "in 10m").
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

// readPending renders the user's outstanding reminders. Never an error: a turn
// with no user, or a user with nothing scheduled, pins nothing.
func readPending(m *scheduler.JobManager) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		s, ok := scope.From(ctx)
		if !ok || !s.HasUser() {
			return "", nil
		}
		jobs := m.ForUser(s.User)
		if len(jobs) == 0 {
			return "", nil
		}
		var b strings.Builder
		b.WriteString("<reminders>\nWaiting to fire. When one does, it arrives as a message to you.\n")
		for i, j := range jobs {
			if i == pendingMax {
				fmt.Fprintf(&b, "…and %d more\n", len(jobs)-pendingMax)
				break
			}
			when := j.Cron
			if when == "" {
				when = scheduler.FormatWhen(j.At)
			}
			name := j.Name
			if name == "" {
				name = j.ID
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", name, when, oneLine(j.Prompt))
		}
		b.WriteString("</reminders>")
		return b.String(), nil
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
