// Package telemetry tags tobee's logs by what they record, so the agent's
// reasoning chain can be read — or filtered — on its own (D-040).
//
// Every log record carries a `cat` attribute:
//
//   - input:    what a turn starts from — the inbound event and any resumed
//     request and question.
//   - thinking: what the model decided — reasoning text, plans, step results.
//   - action:   what the model did — tool calls and their results.
//   - output:   what reached a person — replies, plan announcements, questions.
//   - llm:      each model call — latency, tokens, finish reason, and (at
//     DEBUG) the messages sent and received.
//   - system:   everything else. Applied by Handler to any record that sets
//     no category, so untagged code needs no changes.
//
// Chain records also carry the correlation attributes attached with With —
// `task`, `phase`, and `step` — through a logger carried on the context.
package telemetry

import (
	"context"
	"log/slog"
	"unicode/utf8"
)

// Category is the `cat` attribute value.
type Category string

const (
	Input    Category = "input"
	Thinking Category = "thinking"
	Action   Category = "action"
	Output   Category = "output"
	LLM      Category = "llm"
	System   Category = "system"
)

// Key is the attribute key categories are logged under.
const Key = "cat"

type ctxKey struct{}

// With returns a context whose logger adds args to every record, e.g.
// With(ctx, "task", id) or With(ctx, "phase", "plan").
func With(ctx context.Context, args ...any) context.Context {
	return context.WithValue(ctx, ctxKey{}, Logger(ctx).With(args...))
}

// Logger returns the context's logger, or the default logger.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Log writes one categorized record through the context's logger.
func Log(ctx context.Context, level slog.Level, cat Category, msg string, args ...any) {
	Logger(ctx).Log(ctx, level, msg, append([]any{Key, string(cat)}, args...)...)
}

// contentLimit caps content attributes, in bytes; 0 means unlimited.
var contentLimit = 4000

// SetContentLimit sets the cap Content applies (LOG_CONTENT_LIMIT).
func SetContentLimit(n int) { contentLimit = n }

// Content renders text for a log record: the text, truncated to the
// content limit, plus its full length so truncation is visible.
func Content(key, s string) slog.Attr {
	v := s
	if contentLimit > 0 && len(s) > contentLimit {
		cut := contentLimit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		v = s[:cut] + "…[truncated]"
	}
	return slog.Group("", slog.String(key, v), slog.Int(key+"_chars", len(s)))
}
