package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// Summarizer keeps a rolling compressed summary of each session's older
// turns in `data/sessions/.../current.md`. It is invoked once per turn by
// the agent loop and must be robust to failure — a broken summarizer
// must never prevent the user from getting a reply.
type Summarizer struct {
	client   *llm.Client
	prompt   string // system prompt describing the compression task
	sessions *SessionStore
}

// NewSummarizer constructs a summarizer. The prompt is typically loaded
// from prompts/summarizer.md.
func NewSummarizer(client *llm.Client, prompt string, sessions *SessionStore) *Summarizer {
	return &Summarizer{client: client, prompt: prompt, sessions: sessions}
}

// Update reruns the summarizer over the session transcript and rewrites
// current.md. If the transcript is short enough we skip the LLM call.
func (s *Summarizer) Update(ctx context.Context, key string, recent []llm.Message) error {
	if s == nil || s.client == nil {
		return nil
	}
	transcript := renderTranscript(recent)
	if transcript == "" {
		return nil
	}

	previous := s.sessions.ReadSummary(key)

	user := fmt.Sprintf(`Previous rolling summary (may be empty):
---
%s
---

New transcript to incorporate (most recent last):
---
%s
---

Produce the updated rolling summary.`, previous, transcript)

	slog.Debug("agent: summarizer: begin",
		"key", key, "transcript_chars", len(transcript),
		"previous_chars", len(previous))
	resp, err := s.client.Call(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: s.prompt},
		{Role: llm.RoleUser, Content: user},
	}, nil, llm.ToolChoiceUnset)
	if err != nil {
		return fmt.Errorf("summarizer llm: %w", err)
	}
	out := strings.TrimSpace(resp.Text)
	if out == "" {
		slog.Debug("agent: summarizer: empty output; skipping write", "key", key)
		return nil
	}
	slog.Debug("agent: summarizer: write", "key", key, "summary_chars", len(out))
	return s.sessions.WriteSummary(key, out)
}

// renderTranscript flattens a message list into a plain-text dialogue so
// the summarizer LLM can reason over it. Tool messages are included but
// abbreviated to avoid blowing up the summary prompt.
func renderTranscript(msgs []llm.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(&sb, "USER: %s\n", m.Content)
		case llm.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&sb, "ASSISTANT: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "ASSISTANT->TOOL %s(%s)\n", tc.Function.Name, truncate(tc.Function.Arguments, 200))
			}
		case llm.RoleTool:
			fmt.Fprintf(&sb, "TOOL %s: %s\n", m.Name, truncate(m.Content, 300))
		}
	}
	return sb.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
