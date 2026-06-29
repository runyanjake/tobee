package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// Synthesizer composes the user-facing reply at the end of every turn.
// Its input is the planner's typed Plan artifact (with each step's
// result) plus the original user message — NOT the act-loop's
// assistant messages. Feeding only structured data prevents the
// synthesizer from treating the act loop as a conversation it should
// continue (the "self-talk" failure mode).
type Synthesizer struct {
	client *llm.Client
	ctxb   *ContextBuilder
	prompt string // contents of prompts/synthesizer.md
}

func NewSynthesizer(client *llm.Client, ctxb *ContextBuilder, prompt string) *Synthesizer {
	return &Synthesizer{client: client, ctxb: ctxb, prompt: prompt}
}

// Finalize runs the synthesis LLM call and returns the reply text.
func (s *Synthesizer) Finalize(t *Turn) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("synthesizer: not configured")
	}

	sys := s.ctxb.ComposeSystem(t.Env, s.prompt)
	if r := t.Plan.Render(); r != "" {
		sys += "\n\n" + r
	}
	sys += "\n\n<synthesize>\nThe work above is complete. Render the user's final reply now. No tool calls. Do not plan, announce, ask questions, or say 'I will'.\n</synthesize>"

	// Synth sees only the original user turn plus the system prompt
	// (persona + plan-as-artifact). The act-loop assistant messages
	// are deliberately omitted.
	userTurn := llm.Message{Role: llm.RoleUser, Content: t.Env.Content}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: sys},
		userTurn,
	}

	slog.Debug("agent: synthesizer: begin", "plan_steps", len(t.Plan.Steps))
	logPrompt("agent: synthesizer: prompt", msgs)
	resp, err := s.client.Call(t.Ctx, msgs, nil, llm.ToolChoiceUnset)
	if err != nil {
		return "", fmt.Errorf("synthesizer: llm: %w", err)
	}
	out := strings.TrimSpace(resp.Text)
	slog.Debug("agent: synthesizer: llm response",
		"finish", resp.Finish,
		"text_chars", len(resp.Text),
		"text", resp.Text,
		"tool_calls_count", len(resp.ToolCalls),
		"tool_calls", renderToolCalls(resp.ToolCalls))
	return out, nil
}
