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

// Synthesizer composes the user-facing reply at the end of every turn
// via the reply.commit virtual tool. The Discord message is rendered
// in Go from the tool's structured output — spoken text plus zero or
// more fenced artifacts — so the message shape is deterministic and
// does not depend on the model formatting prose correctly.
type Synthesizer struct {
	client *llm.Client
	ctxb   *ContextBuilder
	prompt string // contents of prompts/synthesizer.md
}

func NewSynthesizer(client *llm.Client, ctxb *ContextBuilder, prompt string) *Synthesizer {
	return &Synthesizer{client: client, ctxb: ctxb, prompt: prompt}
}

// Finalize runs the synthesis LLM call and returns the rendered reply.
// On protocol violation (no reply.commit call) it retries once with a
// nudge and then fails the turn.
func (s *Synthesizer) Finalize(t *Turn) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("synthesizer: not configured")
	}

	sys := s.ctxb.ComposeSystem(t.Env, s.prompt)
	if r := t.Plan.Render(); r != "" {
		sys += "\n\n" + r
	}
	sys += "\n\n<synthesize>\nThe work above is complete. Call reply.commit exactly once to render the user's final reply. No other tools. Do not plan, announce, ask questions, or say 'I will'.\n</synthesize>"

	toolSpec := []llm.ToolSpec{{
		Name:        replyCommitTool,
		Description: "Commit the user-facing reply. `spoken` is the plain-text words you say. `artifacts` are things you're handing over (code, drafts, snippets) — each rendered as a fenced block after the spoken text. Call exactly once.",
		InputSchema: replyCommitSchema,
	}}

	userTurn := llm.Message{Role: llm.RoleUser, Content: t.Env.Content}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: sys},
		userTurn,
	}

	slog.Debug("agent: synthesizer: begin", "plan_steps", len(t.Plan.Steps))

	for attempt := 0; attempt < 2; attempt++ {
		logPrompt("agent: synthesizer: prompt", msgs)
		resp, err := s.client.Call(t.Ctx, msgs, toolSpec, llm.ToolChoiceRequired)
		if err != nil {
			return "", fmt.Errorf("synthesizer: llm: %w", err)
		}
		slog.Debug("agent: synthesizer: llm response",
			"attempt", attempt,
			"finish", resp.Finish,
			"text_chars", len(resp.Text),
			"text", resp.Text,
			"tool_calls_count", len(resp.ToolCalls),
			"tool_calls", renderToolCalls(resp.ToolCalls))

		for _, tc := range resp.ToolCalls {
			if tc.Function.Name != replyCommitTool {
				continue
			}
			var args replyCommitArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return "", fmt.Errorf("synthesizer: decode %s args: %w", replyCommitTool, err)
			}
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

		if attempt == 0 {
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: strings.TrimSpace(resp.Text)},
				llm.Message{Role: llm.RoleUser, Content: synthNudge},
			)
		}
	}

	return "", fmt.Errorf("synthesizer: protocol violation: model did not call %s after 2 attempts", replyCommitTool)
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
