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

// logPrompt emits a debug log of the prompt the agent is about to send
// to the LLM. The system prompt is reported by length only (it can be
// several KB once memory dumps are attached); subsequent messages are
// reported with role + length, and the tail message's content is
// included verbatim because it usually carries the per-call ask
// (current step reminder, replan reason, user message, etc).
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
	tail := msgs[len(msgs)-1]
	slog.Debug(msg,
		"messages", len(msgs),
		"system_chars", systemChars,
		"roles", roles.String(),
		"tail_role", tail.Role,
		"tail", tail.Content)
}
