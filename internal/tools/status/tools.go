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
	"strconv"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
	"github.com/runyanjake/tobee/internal/tools"
)

// defaultWindow is used when the caller omits `window`. One hour is short
// enough to keep the output concise, long enough to catch a meaningful
// burst of background activity.
const defaultWindow = time.Hour

// maxWindow bounds an explicit lookback. Nothing in the reporters retains
// state this long, so a larger window only reads as precision the output
// does not have.
const maxWindow = 30 * 24 * time.Hour

// Register adds status.* tools to reg, backed by reps.
func Register(reg *tools.Registry, reps *abilities.Registry) {
	// A relative duration, not an absolute instant. The model has no
	// clock, so an absolute timestamp it composes lands near its
	// training cutoff — the earlier `since` parameter took an ISO-8601
	// string and got "2023-10-05T18:00:00Z" on a live turn. A duration
	// cannot express that mistake.
	windowSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"window": {"type": "string", "description": "How far back to look, as a duration: \"30m\", \"1h\", \"24h\", \"7d\". Defaults to 1h. Set it only when the user named a period."}
		}
	}`)

	reg.MustRegister(tools.Spec{
		Name: "status.summary",
		Description: "Brief few-sentence overview of tobee's current activity across subsystems. " +
			"Use for general 'how are things?' / 'what are you up to?' inquiries. " +
			"Renders the finished answer itself — calling this tool answers the question. " +
			"Optional window=duration (\"1h\", \"24h\", \"7d\"; default 1h).",
		InputSchema: windowSchema,
		Verbatim:    true,
		Handler:     summaryHandler(reps),
	})

	reg.MustRegister(tools.Spec{
		Name: "status.report",
		Description: "Strict full-detail status block per subsystem (discord, scheduler, schedules, janitor, …). " +
			"Use when the user asks for specifics — failures, schedules, exact next-fire times, channel filters. " +
			"Prefer status.summary for general inquiries. " +
			"Renders the finished answer itself — calling this tool answers the question. " +
			"Optional window=duration (\"1h\", \"24h\", \"7d\"; default 1h).",
		InputSchema: windowSchema,
		Verbatim:    true,
		Handler:     reportHandler(reps),
	})
}

// parseSince resolves the caller's `window` duration into the absolute
// instant the reporters filter on. Returns now-1h when unset.
func parseSince(args json.RawMessage) (time.Time, error) {
	var in struct {
		Window string `json:"window"`
	}
	_ = json.Unmarshal(args, &in)

	d, err := parseWindow(in.Window)
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(-d), nil
}

// parseWindow accepts Go duration syntax plus a "d" (days) suffix, which
// time.ParseDuration does not handle and which is the natural unit for a
// multi-day lookback.
func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultWindow, nil
	}

	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid window %q: want a duration like \"1h\", \"24h\", \"7d\"", s)
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid window %q: want a duration like \"1h\", \"24h\", \"7d\"", s)
		}
	}

	if d <= 0 {
		return 0, fmt.Errorf("window %q must be positive", s)
	}
	if d > maxWindow {
		return 0, fmt.Errorf("window %q exceeds the %d-day maximum; no subsystem retains state that long",
			s, int(maxWindow/(24*time.Hour)))
	}
	return d, nil
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
