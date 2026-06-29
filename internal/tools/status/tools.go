// Package status registers the status.* tools — the model-facing entry
// points to tobee's introspection layer.
//
// Two tools, both returning pre-rendered deterministic text:
//
//   - status.summary — a brief few-sentence overview. Use for general
//     "how are things?" / "what are you up to?" inquiries.
//   - status.report  — a strict full-detail block keyed by subsystem.
//     Use when the user asks for specifics (failures, schedules, exact
//     next-fire times, channel filters, …).
//
// Both tools return text the model is expected to relay verbatim. The
// render shape lives in each Reporter, not in this package.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
	"github.com/runyanjake/tobee/internal/tools"
)

// defaultWindow is used when the caller omits `since`. One hour is short
// enough to keep the output concise, long enough to catch a meaningful
// burst of background activity.
const defaultWindow = time.Hour

// Register adds status.* tools to reg, backed by reps.
func Register(reg *tools.Registry, reps *abilities.Registry) {
	sinceSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"since": {"type": "string", "description": "ISO-8601 timestamp. Recent activity is filtered to >= this time. Defaults to now-1h."}
		}
	}`)

	reg.MustRegister(tools.Spec{
		Name: "status.summary",
		Description: "Brief few-sentence overview of tobee's current activity across subsystems. " +
			"Use for general 'how are things?' / 'what are you up to?' inquiries. " +
			"Returns pre-rendered deterministic text; relay it to the user verbatim, do not rephrase. " +
			"Optional since=ISO-8601 bounds the window (default 1h).",
		InputSchema: sinceSchema,
		Handler:     summaryHandler(reps),
	})

	reg.MustRegister(tools.Spec{
		Name: "status.report",
		Description: "Strict full-detail status block per subsystem (discord, scheduler, schedules, janitor, …). " +
			"Use when the user asks for specifics — failures, schedules, exact next-fire times, channel filters. " +
			"Prefer status.summary for general inquiries. " +
			"Returns pre-rendered deterministic text; relay it to the user verbatim, do not rephrase. " +
			"Optional since=ISO-8601 bounds the window (default 1h).",
		InputSchema: sinceSchema,
		Handler:     reportHandler(reps),
	})
}

func parseSince(args json.RawMessage) (time.Time, error) {
	var in struct {
		Since string `json:"since"`
	}
	_ = json.Unmarshal(args, &in)
	if in.Since == "" {
		return time.Now().Add(-defaultWindow), nil
	}
	t, err := time.Parse(time.RFC3339, in.Since)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid since %q: %w", in.Since, err)
	}
	return t, nil
}

func summaryHandler(reps *abilities.Registry) tools.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		since, err := parseSince(args)
		if err != nil {
			return "", err
		}
		return reps.RenderSummary(ctx, since), nil
	}
}

func reportHandler(reps *abilities.Registry) tools.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		since, err := parseSince(args)
		if err != nil {
			return "", err
		}
		return reps.RenderReport(ctx, since), nil
	}
}
