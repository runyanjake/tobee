// Package status is the built-in "status" MCP server, rendered by each
// abilities.Reporter. Only the report is Verbatim (D-030): a verbatim result is
// appended to the reply whether or not the reply is about it, so a stray
// summary call put "I've handled 3 messages on Discord" at the end of answers
// about shopping lists (D-064).
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
	"github.com/runyanjake/tobee/internal/mcpserver"
)

const defaultWindow = time.Hour

// maxWindow: no reporter retains state longer, so a larger window would imply false precision.
const maxWindow = 30 * 24 * time.Hour

func New(instructions string, reps *abilities.Registry) *mcpserver.Server {
	srv := mcpserver.New("status", instructions)
	// A relative duration, not a timestamp: the model has no clock and guesses its training cutoff.
	windowSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"window": {"type": "string", "description": "How far back to look, as a duration: \"30m\", \"1h\", \"24h\", \"7d\". Defaults to 1h. Set it only when the user named a period."}
		}
	}`)

	srv.Add(mcpserver.Tool{
		Name: "summary",
		Description: "How you yourself have been doing: messages handled, tool calls, failures, what you are waiting on. " +
			"Use for 'how are things?' / 'what are you up to?'. " +
			"Not for the user's own things — their reminders are reminders_list and their notes are in memory. " +
			"Returns the sentence to say; pass it on as written rather than recounting it. " +
			"Say it only if that is what was asked — it is about you, not about anything the user keeps. " +
			"Optional window=duration (\"1h\", \"24h\", \"7d\"; default 1h).",
		InputSchema: windowSchema,
		ReadOnly:    true,
		Handler:     summaryHandler(reps),
	})

	srv.Add(mcpserver.Tool{
		Name: "report",
		Description: "Strict full-detail status block per subsystem (connectors, schedules, ingest, mcp, …), for an operator. " +
			"Use when the user asks about tobee's internals — failures, restarts, exact next-fire times, channel filters. " +
			"Prefer status_summary for general inquiries. " +
			"Renders the finished answer itself — calling this tool answers the question. " +
			"Optional window=duration (\"1h\", \"24h\", \"7d\"; default 1h).",
		InputSchema: windowSchema,
		Verbatim:    true,
		ReadOnly:    true,
		Handler:     reportHandler(reps),
	})
	return srv
}

// parseSince converts `window` into the absolute instant reporters filter on.
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

// parseWindow adds a "d" (days) suffix, which time.ParseDuration lacks.
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

func summaryHandler(reps *abilities.Registry) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		since, err := parseSince(args)
		if err != nil {
			return "", err
		}
		return reps.RenderSummary(ctx, since), nil
	}
}

func reportHandler(reps *abilities.Registry) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		since, err := parseSince(args)
		if err != nil {
			return "", err
		}
		return reps.RenderReport(ctx, since), nil
	}
}
