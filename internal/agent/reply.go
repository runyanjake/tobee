package agent

import (
	"encoding/json"
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
// output appended by code (D-030). Multi-line verbatim is fenced so it
// renders as written.
func renderReply(args replyArgs, verbatim []VerbatimBlock) string {
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
	return sb.String()
}
