package agent

import "testing"

func TestSalvageToolCall(t *testing.T) {
	allowed := []string{"reply.commit", "step.finish", "status.summary"}

	t.Run("recovers the exact text that broke synth", func(t *testing.T) {
		// Verbatim from the 2026-07-19 21:29:02 log line.
		const text = `reply.commit({"spoken":"In the last hour, I've seen 2 new messages on Discord.","artifacts":[]})`
		got, ok := salvageToolCall(text, allowed)
		if !ok {
			t.Fatal("did not salvage a well-formed call")
		}
		if got.Function.Name != "reply.commit" {
			t.Fatalf("name = %q", got.Function.Name)
		}
		if !contains(got.Function.Arguments, `"spoken"`) {
			t.Fatalf("arguments = %q", got.Function.Arguments)
		}
	})

	t.Run("tolerates the retry's extra spacing", func(t *testing.T) {
		const text = `reply.commit({"spoken":"hi", "artifacts": []})`
		if _, ok := salvageToolCall(text, allowed); !ok {
			t.Fatal("did not salvage")
		}
	})

	rejected := []struct {
		name string
		text string
	}{
		{
			// Also from the log. The model narrated a call it never made
			// and invented the result — salvaging this would fabricate.
			name: "prose describing a call",
			text: `Got it. Discord has been active recently, seeing 2 inbound messages in the last hour. How can I help you now? Step.finish called with the result: "Discord has been active, with 2 new messages in the past hour."`,
		},
		{name: "plain conversational text", text: "Tobeel here. In the last hour, I've seen 2 new messages on Discord."},
		{name: "unknown tool", text: `evil.exfiltrate({"path":"/etc/passwd"})`},
		{name: "wrong case is narration", text: `Step.finish({"result":"done"})`},
		{name: "malformed json", text: `reply.commit({"spoken": unquoted})`},
		{name: "non-object argument", text: `reply.commit("just a string")`},
		{name: "no parens", text: "reply.commit"},
		{name: "trailing prose after the call", text: `reply.commit({"spoken":"hi"}) and then I will follow up.`},
		{name: "empty", text: ""},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if got, ok := salvageToolCall(tc.text, allowed); ok {
				t.Fatalf("salvaged %q as %+v, want rejection", tc.text, got)
			}
		})
	}

	t.Run("strips a fenced call", func(t *testing.T) {
		text := "```json\n" + `reply.commit({"spoken":"hi"})` + "\n```"
		if _, ok := salvageToolCall(text, allowed); !ok {
			t.Fatal("did not salvage a fenced call")
		}
	})

	t.Run("empty args become an empty object", func(t *testing.T) {
		got, ok := salvageToolCall("status.summary()", allowed)
		if !ok {
			t.Fatal("did not salvage")
		}
		if got.Function.Arguments != "{}" {
			t.Fatalf("arguments = %q, want {}", got.Function.Arguments)
		}
	})
}

func contains(s, sub string) bool { return len(s) >= len(sub) && stringsIndex(s, sub) >= 0 }

func stringsIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
