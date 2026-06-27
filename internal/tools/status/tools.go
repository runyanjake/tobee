// Package status registers the status.report tool — the model-facing entry
// point to tobee's introspection layer.
//
// status.report takes an optional `since` (ISO-8601) and returns a JSON
// object keyed by reporter name, each value an abilities.ReportData with
// optional Doing / Done / Waiting buckets. The shape is deliberately loose:
// reporters add new fields without coordinating with the tool.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"tobee/internal/abilities"
	"tobee/internal/tools"
)

// defaultWindow is used when the caller omits `since`. One hour is short
// enough to keep the report concise, long enough to catch a meaningful
// burst of background activity.
const defaultWindow = time.Hour

// Register adds status.* tools to reg, backed by reps.
func Register(reg *tools.Registry, reps *abilities.Registry) {
	reg.MustRegister(tools.Spec{
		Name: "status.report",
		Description: `Report what tobee is doing right now, what it has done in the background, ` +
			`and what it is waiting for. Aggregates across registered subsystems (scheduler, ` +
			`janitor, integrations, ...). Pass since=ISO-8601 to bound the "done" window; ` +
			`default is the last 1h.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"since": {"type": "string", "description": "ISO-8601 timestamp. Recent activity is filtered to >= this time. Defaults to now-1h."}
			}
		}`),
		Handler: reportHandler(reps),
	})
}

func reportHandler(reps *abilities.Registry) tools.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Since string `json:"since"`
		}
		_ = json.Unmarshal(args, &in)

		since := time.Now().Add(-defaultWindow)
		if in.Since != "" {
			t, err := time.Parse(time.RFC3339, in.Since)
			if err != nil {
				return "", fmt.Errorf("invalid since %q: %w", in.Since, err)
			}
			since = t
		}

		snap := reps.Snapshot(ctx, since)
		out := struct {
			Since   time.Time                       `json:"since"`
			Now     time.Time                       `json:"now"`
			Reports map[string]abilities.ReportData `json:"reports"`
		}{
			Since:   since,
			Now:     time.Now(),
			Reports: snap,
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal report: %w", err)
		}
		return string(raw), nil
	}
}
