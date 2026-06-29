package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// Synthesizer composes the final user-facing reply for a multi-step
// plan once the executor has run all steps. Single-step plans skip
// this stage and use the step's own result text as the reply.
type Synthesizer struct {
	client *llm.Client
	ctxb   *ContextBuilder
	prompt string // contents of prompts/synthesizer.md
}

func NewSynthesizer(client *llm.Client, ctxb *ContextBuilder, prompt string) *Synthesizer {
	return &Synthesizer{client: client, ctxb: ctxb, prompt: prompt}
}

// Finalize runs the synthesis LLM call and returns the reply text. The
// plan (with each step's result) and the executor transcript are both
// in scope, since the synthesizer persona occupies the system slot and
// the transcript follows verbatim.
func (s *Synthesizer) Finalize(t *Turn) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("synthesizer: not configured")
	}

	sys := s.ctxb.ComposeSystem(t.Env, s.prompt, t.Plan)
	sys += "\n\n<synthesize>\nCompose the user-facing reply now. No tool calls.\n</synthesize>"

	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, t.Transcript...)

	slog.Debug("agent: synthesizer: begin",
		"steps", len(t.Plan.Steps), "transcript_msgs", len(t.Transcript))
	resp, err := s.client.Call(t.Ctx, msgs, nil, llm.ToolChoiceUnset)
	if err != nil {
		return "", fmt.Errorf("synthesizer: llm: %w", err)
	}
	out := strings.TrimSpace(resp.Text)
	slog.Debug("agent: synthesizer: done",
		"finish", resp.Finish, "reply_chars", len(out))
	return out, nil
}
