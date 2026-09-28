package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// decide asks the model for its next tool call and logs only new messages, never the transcript (D-040).
func decide(ctx context.Context, model llm.Model, conv *Conversation, tools []llm.ToolSpec) (*llm.Decision, error) {
	logNewMessages(ctx, conv)

	start := time.Now()
	d, err := model.Decide(ctx, conv.Messages, tools)
	elapsed := time.Since(start).Milliseconds()

	if d != nil {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.LLM, "agent: llm call",
			"duration_ms", elapsed,
			"prompt_tokens", d.Usage.PromptTokens,
			"completion_tokens", d.Usage.CompletionTokens,
			"finish", d.Finish,
			"tool", d.Call.Function.Name)
		telemetry.Log(ctx, slog.LevelDebug, telemetry.LLM, "agent: llm response",
			"raw", d.Raw, "reasoning", d.Reasoning)
		if r := strings.TrimSpace(d.Reasoning); r != "" {
			telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: reasoning", telemetry.Content("content", r))
		}
	}

	switch {
	case errors.Is(err, llm.ErrInvalidDecision):
		raw := ""
		if d != nil {
			raw = d.Raw
		}
		telemetry.Log(ctx, slog.LevelError, telemetry.LLM, "agent: PROTOCOL VIOLATION",
			"err", err, "raw_chars", len(raw), "raw_preview", oneLine(raw))
	case err != nil:
		telemetry.Log(ctx, slog.LevelError, telemetry.LLM, "agent: llm call failed",
			"duration_ms", elapsed, "err", err, "ctx_err", ctx.Err())
	}
	return d, err
}

// logNewMessages logs only messages since the last call; the log still reconstructs the full prompt.
func logNewMessages(ctx context.Context, conv *Conversation) {
	logger := telemetry.Logger(ctx)
	if !logger.Enabled(ctx, slog.LevelDebug) {
		conv.logged = len(conv.Messages)
		return
	}
	for i := conv.logged; i < len(conv.Messages); i++ {
		m := conv.Messages[i]
		attrs := []any{
			"index", i,
			"role", string(m.Role),
			"content", m.Content,
		}
		if m.Name != "" {
			attrs = append(attrs, "name", m.Name)
		}
		if m.ToolCallID != "" {
			attrs = append(attrs, "tool_call_id", m.ToolCallID)
		}
		if len(m.ToolCalls) > 0 {
			attrs = append(attrs, "tool_calls", renderToolCalls(m.ToolCalls))
		}
		telemetry.Log(ctx, slog.LevelDebug, telemetry.LLM, "agent: llm message", attrs...)
	}
	conv.logged = len(conv.Messages)
}

func renderToolCalls(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, tc := range calls {
		parts = append(parts, fmt.Sprintf("%s(%s)", tc.Function.Name, oneLine(tc.Function.Arguments)))
	}
	return strings.Join(parts, "; ")
}

// oneLine collapses whitespace so text survives as a single log field.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}
