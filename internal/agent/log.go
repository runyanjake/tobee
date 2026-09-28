package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// callLLM sends the conversation to the model and records the exchange
// (D-040):
//
//   - DEBUG llm: each message added since the previous call — never the
//     whole transcript again — and the full response;
//   - INFO llm: one summary line with latency, tokens, and finish reason;
//   - INFO thinking: the model's reasoning and any text it wrote.
func callLLM(ctx context.Context, client *llm.Client, conv *Conversation, tools []llm.ToolSpec, choice llm.ToolChoice) (*llm.Response, error) {
	logNewMessages(ctx, conv)

	start := time.Now()
	resp, err := client.Call(ctx, conv.Messages, tools, choice)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		telemetry.Log(ctx, slog.LevelError, telemetry.LLM, "agent: llm call failed",
			"duration_ms", elapsed, "err", err, "ctx_err", ctx.Err())
		return nil, err
	}

	telemetry.Log(ctx, slog.LevelInfo, telemetry.LLM, "agent: llm call",
		"duration_ms", elapsed,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"finish", resp.Finish,
		"tool_calls", toolCallNames(resp.ToolCalls))
	telemetry.Log(ctx, slog.LevelDebug, telemetry.LLM, "agent: llm response",
		"text", resp.Text, "reasoning", resp.Reasoning, "tool_calls", renderToolCalls(resp.ToolCalls))

	if r := strings.TrimSpace(resp.Reasoning); r != "" {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: reasoning", telemetry.Content("content", r))
	}
	if txt := strings.TrimSpace(resp.Text); txt != "" {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: model text", telemetry.Content("content", txt))
	}
	return resp, nil
}

// logNewMessages logs, at DEBUG, the messages appended since the last
// call, so a turn's full prompt is reconstructable from the log without
// re-printing the growing transcript on every call.
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

// logViolation records a phase response that skipped its required tool.
func logViolation(ctx context.Context, attempt int, expected string, resp *llm.Response) {
	telemetry.Log(ctx, slog.LevelError, telemetry.LLM, "agent: PROTOCOL VIOLATION",
		"attempt", attempt,
		"expected_tool", expected,
		"finish", resp.Finish,
		"text_chars", len(resp.Text),
		"text_preview", oneLine(resp.Text),
		"tool_calls", renderToolCalls(resp.ToolCalls))
}

// renderToolCalls flattens tool calls into `name1(args1); name2(args2)`.
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

func toolCallNames(calls []llm.ToolCall) string {
	names := make([]string, 0, len(calls))
	for _, tc := range calls {
		names = append(names, tc.Function.Name)
	}
	return strings.Join(names, ",")
}

// oneLine collapses whitespace in a string so it survives as a single
// log field. Empty input yields empty output.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}
