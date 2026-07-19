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
		args     replyCommitArgs
		verbatim []VerbatimBlock
		want     string
	}{
		{
			name:     "single-line verbatim goes in bare",
			args:     replyCommitArgs{Spoken: "Here's where things stand."},
			verbatim: []VerbatimBlock{{Tool: "status.summary", Body: summary}},
			want:     "Here's where things stand.\n\n" + summary,
		},
		{
			name:     "empty spoken yields the block alone",
			args:     replyCommitArgs{},
			verbatim: []VerbatimBlock{{Tool: "status.summary", Body: summary}},
			want:     summary,
		},
		{
			name:     "multi-line verbatim is fenced",
			args:     replyCommitArgs{},
			verbatim: []VerbatimBlock{{Tool: "status.report", Body: report}},
			want:     "```\n" + report + "\n```",
		},
		{
			name: "verbatim follows model artifacts",
			args: replyCommitArgs{
				Spoken:    "Done.",
				Artifacts: []replyArtifact{{Lang: "go", Body: "package main"}},
			},
			verbatim: []VerbatimBlock{{Tool: "status.summary", Body: summary}},
			want:     "Done.\n\n```go\npackage main\n```\n\n" + summary,
		},
		{
			name: "no verbatim leaves the reply untouched",
			args: replyCommitArgs{Spoken: "Hello."},
			want: "Hello.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderReply(tc.args, tc.verbatim); got != tc.want {
				t.Fatalf("renderReply() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// The verbatim block must survive even when the model ignores the
// instruction to stay quiet — that is the whole point of moving the
// guarantee out of the prompt and into code.
func TestRenderReplyVerbatimSurvivesModelRewording(t *testing.T) {
	const body = "Discord is connected and saw 1 inbound message in the window."
	got := renderReply(
		replyCommitArgs{Spoken: "Discord checked and found 1 inbound message in the last hour."},
		[]VerbatimBlock{{Tool: "status.summary", Body: body}},
	)
	if !strings.Contains(got, body) {
		t.Fatalf("renderReply() dropped the verbatim block:\n%s", got)
	}
}

// When the synthesiser dies, the loop falls back to whatever the tools
// already rendered. On 2026-07-19 that path did not exist and the user
// received an empty message despite status.summary having succeeded.
func TestRenderReplyVerbatimOnlyIsDeliverable(t *testing.T) {
	const body = "Discord is connected and saw 2 inbound messages in the window."
	got := renderReply(replyCommitArgs{}, []VerbatimBlock{{Tool: "status.summary", Body: body}})
	if got != body {
		t.Fatalf("renderReply() = %q, want %q", got, body)
	}
	if got == "" {
		t.Fatal("empty reply would be dropped by deliver")
	}
}

func TestAddVerbatim(t *testing.T) {
	var turn Turn

	turn.AddVerbatim("status.summary", "  all quiet  ")
	turn.AddVerbatim("status.summary", "all quiet") // duplicate
	turn.AddVerbatim("status.report", "")           // blank
	turn.AddVerbatim("status.report", "details")

	if len(turn.Verbatim) != 2 {
		t.Fatalf("Verbatim = %v, want 2 entries", turn.Verbatim)
	}
	if turn.Verbatim[0].Body != "all quiet" {
		t.Fatalf("body not trimmed: %q", turn.Verbatim[0].Body)
	}
}

// The synthesize template branches on HasVerbatim. Parse and render it
// from disk so a broken action fails here rather than at boot.
func TestSynthesizeTemplateBranches(t *testing.T) {
	states, err := LoadStateTemplates(filepath.Join("..", "..", "prompts", "state"))
	if err != nil {
		t.Fatalf("LoadStateTemplates: %v", err)
	}

	with, err := states.Render("synthesize", StateData{HasVerbatim: true})
	if err != nil {
		t.Fatalf("render with verbatim: %v", err)
	}
	without, err := states.Render("synthesize", StateData{})
	if err != nil {
		t.Fatalf("render without verbatim: %v", err)
	}

	if !strings.Contains(with, "already handled") {
		t.Fatalf("HasVerbatim=true did not emit the pre-rendered section:\n%s", with)
	}
	if strings.Contains(without, "already handled") {
		t.Fatalf("HasVerbatim=false leaked the pre-rendered section:\n%s", without)
	}
	if !strings.Contains(without, "reply.commit") {
		t.Fatalf("base contract missing from synthesize template:\n%s", without)
	}
}
