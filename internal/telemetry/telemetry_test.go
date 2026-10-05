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

// The tag is derived from the message, so it can never disagree with the text
// beside it and no call site has to pass one (D-058).
func TestMarker(t *testing.T) {
	tests := []struct {
		msg  string
		cat  Category
		want string
	}{
		{"agent: llm call", LLM, "LLM_CALL"},
		{"agent: tool call", Action, "TOOL_CALL"},
		{"agent: tool result", Action, "TOOL_RESULT"},
		{"agent: tool call repeated; not run", Action, "TOOL_CALL_REPEATED"},
		{"agent: PROTOCOL VIOLATION", LLM, "PROTOCOL_VIOLATION"},
		{"agent: input", Input, "INPUT"},
		{"discord: ambient (not addressed); dropping", System, "AMBIENT_NOT_ADDRESSED"},
		{"jobs: fired", System, "FIRED"},
		// No "subsystem: event" shape: fall back to the category.
		{"tobee is running — press Ctrl+C to exit", System, "SYSTEM"},
		{"", Action, "ACTION"},
		{"", "", "SYSTEM"},
	}
	for _, tc := range tests {
		if got := Marker(tc.msg, tc.cat); got != tc.want {
			t.Errorf("Marker(%q, %q) = %q, want %q", tc.msg, tc.cat, got, tc.want)
		}
	}
}

func TestMarkerIsBounded(t *testing.T) {
	got := Marker("prompts: MISSING — agent will misbehave (check PROMPTS_DIR mount)", System)
	if len(got) > markerMaxLen {
		t.Fatalf("Marker() = %q (%d chars), cap is %d", got, len(got), markerMaxLen)
	}
	if strings.ContainsAny(got, " —()") {
		t.Fatalf("Marker() = %q, want only A-Z0-9_", got)
	}
}

// The original message text survives, so CI's boot-log greps still match.
func TestHandlerKeepsTheMessageText(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler(slog.NewTextHandler(&buf, nil))).Info("tobee is running — press Ctrl+C to exit")
	line := buf.String()
	if !strings.Contains(line, "tobee is running") {
		t.Fatalf("log line lost the message: %s", line)
	}
	if !strings.Contains(line, "[SYSTEM]") {
		t.Fatalf("log line has no marker: %s", line)
	}
	if !strings.Contains(line, "cat=system") {
		t.Fatalf("log line lost its category: %s", line)
	}
}

// A categorised record gets the finer tag and keeps its attributes.
func TestHandlerTagsReasoningRecords(t *testing.T) {
	var buf bytes.Buffer
	slog.SetDefault(slog.New(NewHandler(slog.NewTextHandler(&buf, nil))))
	Log(context.Background(), slog.LevelInfo, Action, "agent: tool call", "tool", "memory_search")
	line := buf.String()
	for _, want := range []string{"[TOOL_CALL]", "agent: tool call", "tool=memory_search", "cat=action"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %q: %s", want, line)
		}
	}
}
