// Package telemetry tags every log record with a `cat` category so the reasoning
// chain can be filtered on its own (D-040).
package telemetry

import (
	"context"
	"log/slog"
	"unicode/utf8"
)

type Category string

const (
	Input    Category = "input"
	Thinking Category = "thinking"
	Action   Category = "action"
	Output   Category = "output"
	LLM      Category = "llm"
	System   Category = "system"
)

const Key = "cat"

type ctxKey struct{}

// With returns a context whose logger adds args (task, phase, step) to every record.
func With(ctx context.Context, args ...any) context.Context {
	return context.WithValue(ctx, ctxKey{}, Logger(ctx).With(args...))
}

func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func Log(ctx context.Context, level slog.Level, cat Category, msg string, args ...any) {
	Logger(ctx).Log(ctx, level, msg, append([]any{Key, string(cat)}, args...)...)
}

// In bytes; 0 means unlimited.
var contentLimit = 4000

func SetContentLimit(n int) { contentLimit = n }

// Content truncates s to the limit and logs its full length so truncation is visible.
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
