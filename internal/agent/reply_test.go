package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderReplyVerbatim(t *testing.T) {
	const summary = "Discord is connected and saw 1 inbound message in the window."
	report := "tobee status — window A → B\n\n## discord\nDoing: connected\nDone: 1 inbound"

	cases := []struct {
		name     string
		args     replyArgs
		verbatim []VerbatimBlock
		want     string
	}{
		{
			name:     "single-line verbatim goes in bare",
			args:     replyArgs{Spoken: "Here's where things stand."},
			verbatim: []VerbatimBlock{{Tool: "status_summary", Body: summary}},
			want:     "Here's where things stand.\n\n" + summary,
		},
		{
			name:     "empty spoken yields the block alone",
			args:     replyArgs{},
			verbatim: []VerbatimBlock{{Tool: "status_summary", Body: summary}},
			want:     summary,
		},
		{
			name:     "multi-line verbatim is fenced",
			args:     replyArgs{},
			verbatim: []VerbatimBlock{{Tool: "status_report", Body: report}},
			want:     "```\n" + report + "\n```",
		},
		{
			name: "verbatim follows model artifacts",
			args: replyArgs{
				Spoken:    "Done.",
				Artifacts: []replyArtifact{{Lang: "go", Body: "package main"}},
			},
			verbatim: []VerbatimBlock{{Tool: "status_summary", Body: summary}},
			want:     "Done.\n\n```go\npackage main\n```\n\n" + summary,
		},
		{
			name: "no verbatim leaves the reply untouched",
			args: replyArgs{Spoken: "Hello."},
			want: "Hello.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderReply(tc.args, tc.verbatim, nil, nil); got != tc.want {
				t.Fatalf("renderReply() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// The verbatim block survives even when the model ignores the prompt (D-030).
func TestRenderReplyVerbatimSurvivesModelRewording(t *testing.T) {
	const body = "Discord is connected and saw 1 inbound message in the window."
	got := renderReply(
		replyArgs{Spoken: "Discord checked and found 1 inbound message in the last hour."},
		[]VerbatimBlock{{Tool: "status_summary", Body: body}},
		nil, nil,
	)
	if !strings.Contains(got, body) {
		t.Fatalf("renderReply() dropped the verbatim block:\n%s", got)
	}
}

// When the synthesiser dies, the loop falls back to what the tools already rendered.
func TestRenderReplyVerbatimOnlyIsDeliverable(t *testing.T) {
	const body = "Discord is connected and saw 2 inbound messages in the window."
	got := renderReply(replyArgs{}, []VerbatimBlock{{Tool: "status.summary", Body: body}}, nil, nil)
	if got != body {
		t.Fatalf("renderReply() = %q, want %q", got, body)
	}
	if got == "" {
		t.Fatal("empty reply would be dropped by deliver")
	}
}

func TestAddVerbatim(t *testing.T) {
	var turn Turn

	turn.AddVerbatim("status_summary", "  all quiet  ")
	turn.AddVerbatim("status_summary", "all quiet") // duplicate
	turn.AddVerbatim("status_report", "")           // blank
	turn.AddVerbatim("status_report", "details")

	if len(turn.Verbatim) != 2 {
		t.Fatalf("Verbatim = %v, want 2 entries", turn.Verbatim)
	}
	if turn.Verbatim[0].Body != "all quiet" {
		t.Fatalf("body not trimmed: %q", turn.Verbatim[0].Body)
	}
}

// The turn directive must load and render from disk, or boot fails later.
func TestTurnTemplateRenders(t *testing.T) {
	states, err := LoadStateTemplates(filepath.Join("..", "..", "prompts", "state"))
	if err != nil {
		t.Fatalf("LoadStateTemplates: %v", err)
	}
	got, err := states.RenderPhase("turn", StateData{})
	if err != nil {
		t.Fatalf("render turn: %v", err)
	}
	if !strings.HasPrefix(got, `<phase name="turn">`) || !strings.Contains(got, "reply") {
		t.Fatalf("turn directive = %q", got)
	}
}

// A turn that went wrong says so in code, whatever the model claims. The
// failure this covers: 12 steps spent repeating one read, then a confident
// "I couldn't find any reminders" (D-051).
func TestRenderReplyAppendsWhatWentWrong(t *testing.T) {
	got := renderReply(
		replyArgs{Spoken: "I couldn't find any reminders for you to clear today."},
		nil, nil,
		[]Problem{
			{Kind: "repeated", Tool: "schedule_list", Detail: "called again with the same arguments; not run a second time"},
			{Kind: "budget", Detail: "ran out of steps after 12 model calls"},
		},
	)
	for _, want := range []string{
		"I couldn't find any reminders",
		"⚠️ Didn't finish cleanly:",
		"• schedule_list: called again with the same arguments",
		"• ran out of steps after 12 model calls, so the request may be unfinished",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderReply() missing %q:\n%s", want, got)
		}
	}
}

// A tool error on a read produces no action line (D-047 covers writes), so
// without this the user hears nothing about it.
func TestRenderReplyReportsReadFailures(t *testing.T) {
	got := renderReply(replyArgs{Spoken: "Here's what I have."}, nil, nil,
		[]Problem{{Kind: "tool_error", Tool: "memory_search", Detail: "scope \"user\" needs a user"}})
	if !strings.Contains(got, "• memory_search: failed — scope \"user\" needs a user") {
		t.Fatalf("renderReply() missing the read failure:\n%s", got)
	}
}

func TestAddProblemKeepsOneEntryPerToolAndKind(t *testing.T) {
	var turn Turn
	turn.AddProblem("tool_error", "memory_write", "disk full")
	turn.AddProblem("tool_error", "memory_write", "disk full again")
	turn.AddProblem("tool_error", "memory_read", "missing")
	turn.AddProblem("budget", "", "out of steps")

	if len(turn.Problems) != 3 {
		t.Fatalf("Problems = %+v, want 3 entries", turn.Problems)
	}
	if turn.Problems[0].Detail != "disk full again" {
		t.Fatalf("Problems[0].Detail = %q, want the latest detail", turn.Problems[0].Detail)
	}
}

// A retry that worked is session history, not something to warn the user
// about; the same problem recurring is (D-051).
func TestRecoveredProblemsAreNotShown(t *testing.T) {
	var turn Turn
	turn.AddRecovered("unreadable", "", "retried")
	if got := renderProblems(turn.Problems); got != "" {
		t.Fatalf("renderProblems() = %q, want nothing for a recovered problem", got)
	}
	if len(turn.Problems) != 1 {
		t.Fatalf("Problems = %+v, want the recovered entry kept for the session", turn.Problems)
	}

	turn.AddProblem("unreadable", "", "failed twice")
	if got := renderProblems(turn.Problems); !strings.Contains(got, "wasn't usable") {
		t.Fatalf("renderProblems() = %q, want the promoted problem shown", got)
	}
}
