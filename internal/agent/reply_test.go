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
			{Kind: "repeated", Tool: "schedule_list", Detail: "I kept calling schedule_list the same way (3 times), so I stopped using it"},
			{Kind: "budget", Detail: "I ran out of steps after 12 tries, so this may be unfinished"},
		},
	)
	// Every line speaks as tobee about what it tried (D-053).
	for _, want := range []string{
		"I couldn't find any reminders",
		"⚠️ I didn't finish this cleanly:",
		"• I kept calling schedule_list the same way (3 times), so I stopped using it",
		"• I ran out of steps after 12 tries, so this may be unfinished",
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
		[]Problem{{Kind: "tool_error", Tool: "memory_grep", Detail: `I tried memory_grep and it failed: scope "user" needs a user`}})
	if !strings.Contains(got, `• I tried memory_grep and it failed: scope "user" needs a user`) {
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

	turn.AddProblem("unreadable", "", "I couldn't put together a usable tool call, twice over")
	if got := renderProblems(turn.Problems); !strings.Contains(got, "I couldn't put together a usable tool call") {
		t.Fatalf("renderProblems() = %q, want the promoted problem shown", got)
	}
}

// Asked for its status, the model answered by quoting a sentence out of the
// status_summary result it had just read — and code appends that result to the
// reply anyway, so the user got the same line twice, the second time inside a
// longer block. The harness note telling it not to repeat the output was in
// context and ignored (D-062).
func TestRenderReplyDropsSpokenAlreadyInVerbatim(t *testing.T) {
	const body = "I've handled 7 messages on Discord. I've made 9 tool calls since starting up. I'm holding 2 reminders for you."

	cases := []struct {
		name   string
		spoken string
		want   string
	}{
		{"quoted one sentence out of it", "I'm holding 2 reminders for you.", body},
		{"quoted the whole thing", body, body},
		{"requoted with different case and spacing", "i'm  holding 2 REMINDERS for you.", body},
		{"said something of its own", "Here's where things stand.", "Here's where things stand.\n\n" + body},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderReply(replyArgs{Spoken: c.spoken},
				[]VerbatimBlock{{Tool: "status_summary", Body: body}}, nil, nil)
			if got != c.want {
				t.Fatalf("renderReply() = %q, want %q", got, c.want)
			}
		})
	}
}

// Dropping a line the user needed is worse than repeating one, so the test is
// containment after normalising case and spacing — nothing fuzzier.
func TestSubsumedByVerbatimStaysNarrow(t *testing.T) {
	const body = "Discord is connected and saw 1 inbound message in the window."
	verbatim := []VerbatimBlock{{Tool: "status_summary", Body: body}}

	for _, spoken := range []string{
		"Discord checked and found 1 inbound message in the last hour.", // reworded, not quoted
		"Discord is connected, and there's one more thing worth saying.",
		"",
	} {
		if subsumedByVerbatim(spoken, verbatim) {
			t.Fatalf("%q was treated as a duplicate", spoken)
		}
	}
	if !subsumedByVerbatim("Discord is connected", verbatim) {
		t.Fatal("a quoted fragment was not recognised")
	}
	// Nothing to be subsumed by.
	if subsumedByVerbatim("Anything at all", nil) {
		t.Fatal("spoken was dropped with no verbatim block present")
	}
}
