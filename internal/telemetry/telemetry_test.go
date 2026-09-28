package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func capture(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	return &buf, func() { slog.SetDefault(prev) }
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad JSON %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCategoriesAndCorrelation(t *testing.T) {
	buf, restore := capture(t)
	defer restore()

	ctx := With(context.Background(), "task", "t-1")
	ctx = With(ctx, "phase", "plan")
	Log(ctx, slog.LevelInfo, Thinking, "agent: plan", "goal", "g")
	slog.Info("jobs: loaded")

	recs := records(t, buf)
	if recs[0]["cat"] != "thinking" || recs[0]["task"] != "t-1" || recs[0]["phase"] != "plan" {
		t.Fatalf("chain record = %v", recs[0])
	}
	if recs[1]["cat"] != "system" {
		t.Fatalf("untagged record = %v, want cat=system", recs[1])
	}
}

// A category attached with Logger.With must not be overridden by the default.
func TestHandlerRespectsAttachedCategory(t *testing.T) {
	buf, restore := capture(t)
	defer restore()

	slog.Default().With(Key, string(Output)).Info("sent")
	if recs := records(t, buf); recs[0]["cat"] != "output" {
		t.Fatalf("record = %v", recs[0])
	}
}

func TestContentTruncates(t *testing.T) {
	buf, restore := capture(t)
	defer restore()
	defer SetContentLimit(contentLimit)
	SetContentLimit(5)

	Log(context.Background(), slog.LevelInfo, Input, "in", Content("content", "héllo world"))
	rec := records(t, buf)[0]
	if rec["content"] != "héll…[truncated]" || rec["content_chars"] != float64(len("héllo world")) {
		t.Fatalf("record = %v", rec)
	}
}
