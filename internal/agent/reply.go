package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

var replySchema = json.RawMessage(`{
  "type": "object",
  "required": ["spoken"],
  "properties": {
    "spoken": {"type": "string", "description": "What you say to the user, in your voice. Plain text, no code fences. May be empty if the reply is only artifacts."},
    "artifacts": {
      "type": "array",
      "description": "Things you hand over rather than say: code, drafts, file contents, commands. Each is shown in its own code block after spoken.",
      "items": {
        "type": "object",
        "required": ["body"],
        "properties": {
          "lang": {"type": "string", "description": "Optional language hint, e.g. go, json, sh."},
          "body": {"type": "string"}
        }
      }
    }
  }
}`)

type replyArtifact struct {
	Lang string `json:"lang"`
	Body string `json:"body"`
}

type replyArgs struct {
	Spoken    string          `json:"spoken"`
	Artifacts []replyArtifact `json:"artifacts"`
}

// renderReply writes spoken, then each artifact fenced, then verbatim tool
// output (D-030), one line per action taken (D-047), and finally what didn't
// work (D-051) — everything after the artifacts by code, so what the user
// reads about the world matches what happened.
func renderReply(args replyArgs, verbatim []VerbatimBlock, actions []Action, problems []Problem) string {
	var sb strings.Builder
	if spoken := strings.TrimSpace(args.Spoken); spoken != "" && !subsumedByVerbatim(spoken, verbatim) {
		sb.WriteString(spoken)
	}
	for _, a := range args.Artifacts {
		body := strings.TrimRight(a.Body, "\n")
		if body == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("```")
		sb.WriteString(strings.TrimSpace(a.Lang))
		sb.WriteByte('\n')
		sb.WriteString(body)
		sb.WriteString("\n```")
	}
	for _, v := range verbatim {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		if strings.Contains(v.Body, "\n") {
			sb.WriteString("```\n")
			sb.WriteString(v.Body)
			sb.WriteString("\n```")
		} else {
			sb.WriteString(v.Body)
		}
	}
	if len(actions) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		for i, a := range actions {
			if i > 0 {
				sb.WriteByte('\n')
			}
			mark := "✅"
			if !a.OK {
				mark = "❌"
			}
			fmt.Fprintf(&sb, "%s %s: %s", mark, a.Tool, a.Result)
		}
	}
	if block := renderProblems(problems); block != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(block)
	}
	return sb.String()
}

// subsumedByVerbatim reports whether the spoken line says nothing the verbatim
// block is about to say anyway. Verbatim output is appended to the reply by
// code (D-030), and the model — having just read that output as a tool result
// — answers by quoting a line out of it, so the user reads the same sentence
// twice. The harness already asks it not to and it does anyway; a prompt
// instruction is not a guard (D-051), so drop the duplicate here (D-062).
func subsumedByVerbatim(spoken string, verbatim []VerbatimBlock) bool {
	want := normalizeSpoken(spoken)
	if want == "" {
		return false
	}
	for _, v := range verbatim {
		if strings.Contains(normalizeSpoken(v.Body), want) {
			return true
		}
	}
	return false
}

// normalizeSpoken lowercases and collapses whitespace, so wording that differs
// only in spacing or case still counts as the same sentence. Deliberately not
// fuzzier than that: dropping a line the user needed is worse than repeating one.
func normalizeSpoken(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// renderProblems is the code-written account of a turn that didn't work: the
// model's own words can't be trusted to carry it, as a turn that spent every
// step repeating one read still reported "no reminders to clear" (D-051).
// Each Detail is already a first-person sentence, written where the fact is
// known, so this only has to frame them (D-053).
func renderProblems(problems []Problem) string {
	if len(problems) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, p := range problems {
		if p.Recovered {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("⚠️ I didn't finish this cleanly:")
		}
		sb.WriteString("\n• ")
		sb.WriteString(strings.TrimSpace(p.Detail))
	}
	return sb.String()
}
