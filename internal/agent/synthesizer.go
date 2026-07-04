package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

const replyCommitTool = "reply.commit"

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

const synthNudge = "PROTOCOL VIOLATION: your previous response was not a reply.commit tool call. You must call reply.commit exactly once with the reply's spoken text and any artifacts. Free-form text is not accepted. Retry."

// Synthesizer runs against the shared Conversation to produce the
// user-facing reply. Appends the rendered synthesize state template
// as a user message, then requires reply.commit with
// tool_choice=required. The Discord message text is composed in Go by
// renderReply from the structured output — spoken + fenced artifacts.
type Synthesizer struct {
	client *llm.Client
	states *StateTemplates
}

func NewSynthesizer(client *llm.Client, states *StateTemplates) *Synthesizer {
	return &Synthesizer{client: client, states: states}
}

// Finalize appends the synth state message to the conversation, runs
// the LLM call, and returns the rendered reply. On protocol violation
// it retries once with a nudge and then fails.
func (s *Synthesizer) Finalize(t *Turn) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("synthesizer: not configured")
	}

	userMsg, err := s.states.Render("synthesize", StateData{
		Plan: t.Conversation.Plan,
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

	slog.Debug("agent: synthesizer: begin", "plan_steps", len(t.Conversation.Plan.Steps))

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		logPrompt("agent: synthesizer: prompt", t.Conversation.Messages)
		resp, err := s.client.Call(t.Ctx, t.Conversation.Messages, toolSpec, llm.ToolChoiceRequired)
		if err != nil {
			slog.Error("agent: synthesizer: LLM ERROR",
				"attempt", attempt, "err", err, "ctx_err", t.Ctx.Err())
			lastErr = err
			if t.Ctx.Err() != nil {
				return "", fmt.Errorf("synthesizer: llm: %w", err)
			}
			continue
		}
		logResponse("agent: synthesizer: llm response", resp, "attempt", attempt)

		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		t.Conversation.Append(asst)

		for _, tc := range resp.ToolCalls {
			if tc.Function.Name != replyCommitTool {
				continue
			}
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
			return renderReply(args), nil
		}

		slog.Error("agent: synthesizer: PROTOCOL VIOLATION",
			"attempt", attempt,
			"expected_tool", replyCommitTool,
			"finish", resp.Finish,
			"text_chars", len(resp.Text),
			"text_preview", oneLine(resp.Text),
			"tool_calls_count", len(resp.ToolCalls),
			"tool_calls", renderToolCalls(resp.ToolCalls))
		lastErr = fmt.Errorf("protocol violation: no %s call", replyCommitTool)

		if attempt == 0 {
			t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: synthNudge})
		}
	}

	return "", fmt.Errorf("synthesizer: exhausted retries: %w", lastErr)
}

// renderReply composes the final Discord message from the structured
// reply.commit output: spoken text on top, each artifact as a fenced
// block with an optional language hint. Empty spoken + empty artifacts
// yields an empty string, which the deliver step logs as a failure.
func renderReply(args replyCommitArgs) string {
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
	return sb.String()
}
