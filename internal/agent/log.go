package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// renderToolCalls flattens an LLM response's tool calls into a single
// log-line string: `name1(args1); name2(args2)`. Returns "" for none.
// Arguments are passed through verbatim — they are already JSON.
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

// oneLine collapses whitespace in a string so it survives as a single
// log field. Empty input yields empty output.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// logPrompt emits a debug log of the prompt the agent is about to
// send to the LLM. One summary line with counts + per-role sizes, then
// one line per message with the full content (and tool_calls / tool
// metadata when present) so the exact conversation sent to the model
// is auditable in the console at LOG_LEVEL=DEBUG.
func logPrompt(msg string, msgs []llm.Message) {
	if len(msgs) == 0 {
		slog.Debug(msg, "messages", 0)
		return
	}
	systemChars := 0
	if msgs[0].Role == llm.RoleSystem {
		systemChars = len(msgs[0].Content)
	}
	var roles strings.Builder
	for i, m := range msgs {
		if i > 0 {
			roles.WriteString(", ")
		}
		fmt.Fprintf(&roles, "%s:%d", m.Role, len(m.Content))
	}
	slog.Debug(msg,
		"messages", len(msgs),
		"system_chars", systemChars,
		"roles", roles.String())

	for i, m := range msgs {
		attrs := []any{
			"index", i,
			"of", len(msgs),
			"role", string(m.Role),
			"content_chars", len(m.Content),
			"content", m.Content,
		}
		if m.Name != "" {
			attrs = append(attrs, "name", m.Name)
		}
		if m.ToolCallID != "" {
			attrs = append(attrs, "tool_call_id", m.ToolCallID)
		}
		if len(m.ToolCalls) > 0 {
			attrs = append(attrs,
				"tool_calls_count", len(m.ToolCalls),
				"tool_calls", renderToolCalls(m.ToolCalls))
		}
		slog.Debug(msg+": msg", attrs...)
	}
}

// logResponse emits a debug log of an LLM response. Mirrors logPrompt's
// verbosity — full text + all tool calls at DEBUG so a session's
// prompt/response pair is reconstructable from the console log.
func logResponse(msg string, resp *llm.Response, extra ...any) {
	if resp == nil {
		slog.Debug(msg, append([]any{"nil", true}, extra...)...)
		return
	}
	attrs := []any{
		"finish", resp.Finish,
		"text_chars", len(resp.Text),
		"text", resp.Text,
		"tool_calls_count", len(resp.ToolCalls),
		"tool_calls", renderToolCalls(resp.ToolCalls),
	}
	attrs = append(attrs, extra...)
	slog.Debug(msg, attrs...)
}
