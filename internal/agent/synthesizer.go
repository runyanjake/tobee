package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// Synthesizer composes the user-facing reply at the end of every turn.
// It reads whatever the act loop produced (tool calls, tool results,
// terminal text — or just terminal text for trivial turns) and turns
// it into one outbound message in tobee's voice.
//
// The act loop is the model's scratchpad; the synthesizer is the only
// thing the user sees. Tone, length, and formatting are enforced here.
type Synthesizer struct {
	client *llm.Client
	ctxb   *ContextBuilder
	prompt string // contents of prompts/synthesizer.md
}

func NewSynthesizer(client *llm.Client, ctxb *ContextBuilder, prompt string) *Synthesizer {
	return &Synthesizer{client: client, ctxb: ctxb, prompt: prompt}
}

// Finalize runs the synthesis LLM call and returns the reply text. The
// transcript (including the act loop's terminal assistant message and
// any tool results) is in scope; the synthesizer persona occupies the
// system slot.
func (s *Synthesizer) Finalize(t *Turn) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("synthesizer: not configured")
	}

	sys := s.ctxb.ComposeSystem(t.Env, s.prompt)
	sys += "\n\n<synthesize>\nCompose the user-facing reply now. No tool calls.\n</synthesize>"

	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, t.Transcript...)

	slog.Debug("agent: synthesizer: begin", "transcript_msgs", len(t.Transcript))
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
