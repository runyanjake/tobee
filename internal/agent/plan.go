package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Plan is the checklist the model keeps for multi-step work via the plan
// tool. It exists only for the turn and only drives the progress message.
type Plan struct {
	Goal  string     `json:"goal"`
	Items []PlanItem `json:"steps"`
}

type PlanItem struct {
	Title  string `json:"title"`
	Status string `json:"status"` // pending | active | done | skipped
}

var planSchema = json.RawMessage(`{
  "type": "object",
  "required": ["goal", "steps"],
  "properties": {
    "goal": {"type": "string", "description": "What the user asked for, in one sentence."},
    "steps": {
      "type": "array",
      "minItems": 2,
      "items": {
        "type": "object",
        "required": ["title", "status"],
        "properties": {
          "title": {"type": "string", "description": "The outcome this step produces."},
          "status": {"type": "string", "enum": ["pending", "active", "done", "skipped"]}
        }
      }
    }
  }
}`)

// minShownItems keeps one-step "plans" out of the chat: they add a message
// and say nothing the reply won't.
const minShownItems = 2

func (p *Plan) Render() string {
	var sb strings.Builder
	if goal := strings.TrimSpace(p.Goal); goal != "" {
		fmt.Fprintf(&sb, "**Working on:** %s\n\n", goal)
	} else {
		sb.WriteString("**Working on it.**\n\n")
	}
	for i, it := range p.Items {
		fmt.Fprintf(&sb, "%s %d. %s\n", statusEmoji(it.Status), i+1, strings.TrimSpace(it.Title))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func statusEmoji(s string) string {
	switch s {
	case "active":
		return "🔄"
	case "done":
		return "✅"
	case "skipped":
		return "⏭️"
	default:
		return "⏳"
	}
}
