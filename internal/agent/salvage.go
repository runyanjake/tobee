package agent

import (
	"encoding/json"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// salvageToolCall recovers a tool call the model wrote as message text
// instead of emitting on the native tool_calls channel.
//
// The loaded LM Studio model ignores tool_choice="required" once a
// conversation grows: it produced the literal text
//
//	reply.commit({"spoken":"…","artifacts":[]})
//
// with finish="stop" and no tool calls, twice in a row, which failed the
// synth phase and cost the user any reply at all. The model knew the
// tool and the arguments; only the transport was wrong. D-025 forbids
// accepting free-form prose as an answer, and this does not do that —
// see D-031.
//
// The match is deliberately strict. The whole message, once fences are
// stripped, must be exactly one `name({…})` call naming an allowed tool
// with a parseable JSON object argument. Prose that merely mentions a
// call — "Step.finish called with the result: …", which this same model
// also produced — must not parse, because its "arguments" are a
// summary the model invented rather than a payload it committed to.
func salvageToolCall(text string, allowed []string) (llm.ToolCall, bool) {
	s := stripFences(strings.TrimSpace(text))

	open := strings.Index(s, "(")
	if open < 0 || !strings.HasSuffix(s, ")") {
		return llm.ToolCall{}, false
	}

	name := strings.TrimSpace(s[:open])
	if !allowedTool(name, allowed) {
		return llm.ToolCall{}, false
	}

	args := strings.TrimSpace(s[open+1 : len(s)-1])
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return llm.ToolCall{}, false
	}
	// Arguments must be an object; a bare string or number means we
	// matched prose that happened to have parens.
	var probe map[string]any
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		return llm.ToolCall{}, false
	}

	return llm.ToolCall{
		ID:   "salvaged_" + name,
		Type: "function",
		Function: llm.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}, true
}

// allowedTool matches case-sensitively. A model that writes "Step.finish"
// when the tool is "step.finish" is narrating, not calling.
func allowedTool(name string, allowed []string) bool {
	for _, a := range allowed {
		if name == a {
			return true
		}
	}
	return false
}

// stripFences removes a single surrounding triple-backtick fence, which
// models often wrap around a call they have decided to "show" instead
// of make.
func stripFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}
