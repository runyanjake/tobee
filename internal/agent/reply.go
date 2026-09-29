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
// output (D-030), then one line per action taken (D-047) — the last two by
// code, so what the user reads about the world matches what happened.
func renderReply(args replyArgs, verbatim []VerbatimBlock, actions []Action) string {
	var sb strings.Builder
	if spoken := strings.TrimSpace(args.Spoken); spoken != "" {
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
	return sb.String()
}
