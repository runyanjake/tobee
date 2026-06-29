package agent

import (
	"fmt"
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
