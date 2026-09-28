package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/telemetry"
)

const replyCommitTool = "reply_commit"

var replyCommitSchema = json.RawMessage(`{
  "type": "object",
  "required": ["spoken"],
  "properties": {
    "spoken": {"type": "string", "description": "The plain-text words you say to the user, in tobee's voice. No fences, no headings, no meta-commentary. May be empty if the reply is only an artifact."},
    "artifacts": {
      "type": "array",
      "description": "Zero or more things you are handing over rather than saying: code, drafted messages, poems, config snippets, quotes, commands, file contents, structured data. Each renders as a fenced code block after the spoken text.",
      "items": {
        "type": "object",
        "required": ["body"],
        "properties": {
          "lang": {"type": "string", "description": "Optional language hint for the fence (e.g. 'go', 'json', 'sh'). Omit or empty for a bare fence."},
          "body": {"type": "string", "description": "The artifact contents. Rendered verbatim inside a triple-backtick fence."}
        }
      }
    }
  }
}`)

type replyArtifact struct {
	Lang string `json:"lang"`
	Body string `json:"body"`
}

type replyCommitArgs struct {
	Spoken    string          `json:"spoken"`
	Artifacts []replyArtifact `json:"artifacts"`
}

const synthNudge = "Your last response could not be read as a reply_commit call. Call reply_commit."

// Synthesizer offers only reply_commit; renderReply composes the reply text in Go.
type Synthesizer struct {
	model  llm.Model
	states *StateTemplates
}

func NewSynthesizer(model llm.Model, states *StateTemplates) *Synthesizer {
	return &Synthesizer{model: model, states: states}
}

// Finalize retries once with a nudge on a protocol violation, then fails.
func (s *Synthesizer) Finalize(t *Turn) (string, error) {
	if s == nil || s.model == nil {
		return "", fmt.Errorf("synthesizer: not configured")
	}
	ctx := telemetry.With(t.Ctx, "phase", "synthesize")

	userMsg, err := s.states.RenderPhase("synthesize", StateData{
		Plan:        t.Conversation.Plan,
		HasVerbatim: len(t.Verbatim) > 0,
	})
	if err != nil {
		return "", fmt.Errorf("synthesizer: render synthesize state: %w", err)
	}
	t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: userMsg})

	toolSpec := []llm.ToolSpec{{
		Name:        replyCommitTool,
		Description: "Commit the user-facing reply. `spoken` is the plain-text words you say. `artifacts` are things you're handing over (code, drafts, snippets) — each rendered as a fenced block after the spoken text. Call exactly once.",
		InputSchema: replyCommitSchema,
	}}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		d, err := decide(ctx, s.model, t.Conversation, toolSpec)
		if errors.Is(err, llm.ErrInvalidDecision) {
			lastErr = err
			if attempt == 0 {
				t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: synthNudge})
			}
			continue
		}
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return "", fmt.Errorf("synthesizer: llm: %w", err)
			}
			continue
		}

		tc := d.Call
		t.Conversation.Append(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}})
		var args replyCommitArgs
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("synthesizer: decode %s args: %w", replyCommitTool, err)
		}
		t.Conversation.Append(llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    "ok",
		})
		return renderReply(args, t.Verbatim), nil
	}

	return "", fmt.Errorf("synthesizer: exhausted retries: %w", lastErr)
}

// renderReply appends verbatim blocks in code (D-030): single-line bare, multi-line fenced.
// An empty result is logged as a failure by deliver.
func renderReply(args replyCommitArgs, verbatim []VerbatimBlock) string {
	var sb strings.Builder
	spoken := strings.TrimSpace(args.Spoken)
	if spoken != "" {
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
