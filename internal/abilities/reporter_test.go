package abilities

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fake struct {
	name, full, summary string
}

func (f fake) Name() string { return f.name }

func (f fake) Render(context.Context, time.Time) (string, string) { return f.full, f.summary }

// status_summary is read by a person and delivered verbatim (D-030), so it
// speaks as tobee and carries activity only — a silent reporter says nothing
// rather than reporting its inventory (D-053).
func TestRenderSummaryJoinsWhatHappened(t *testing.T) {
	r := NewRegistry()
	r.Register(fake{name: "discord", summary: "I've handled 2 messages on Discord"})
	r.Register(fake{name: "mcp", summary: "I've made 5 tool calls since starting up"})
	r.Register(fake{name: "ingest", summary: ""}) // nothing wrong: stays quiet
	r.Register(fake{name: "tasks", summary: ""})

	got := r.RenderSummary(context.Background(), time.Time{})
	want := "I've handled 2 messages on Discord. I've made 5 tool calls since starting up."
	if got != want {
		t.Fatalf("RenderSummary() = %q, want %q", got, want)
	}
	if strings.Contains(got, "server") || strings.Contains(got, "source") {
		t.Fatalf("RenderSummary() leaked inventory into a user-facing line: %q", got)
	}
}

func TestRenderSummaryIdle(t *testing.T) {
	r := NewRegistry()
	r.Register(fake{name: "discord", summary: ""})
	got := r.RenderSummary(context.Background(), time.Time{})
	if !strings.HasPrefix(got, "I've been idle") {
		t.Fatalf("RenderSummary() = %q, want a first-person idle line", got)
	}
}

// status_report stays the operator's view: every reporter appears, even silent ones.
func TestRenderReportShowsEveryReporter(t *testing.T) {
	r := NewRegistry()
	r.Register(fake{name: "mcp", full: "Doing:\n  - memory (trusted): 5 tools\n"})
	r.Register(fake{name: "ingest", full: ""})

	got := r.RenderReport(context.Background(), time.Now().Add(-time.Hour))
	for _, want := range []string{"## mcp", "memory (trusted): 5 tools", "## ingest", "(idle)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("RenderReport() missing %q:\n%s", want, got)
		}
	}
}
