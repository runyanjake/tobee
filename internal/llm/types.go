package llm

import (
	"encoding/json"
)

// Role identifies who produced a message in a chat turn.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a single entry in the chat transcript sent to the LLM.
// For RoleTool, ToolCallID must be set and Content carries the tool output.
// For RoleAssistant, ToolCalls may be populated when the model requested tools.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// MarshalJSON always emits a content field. OpenAI-compatible servers
// (LM Studio in particular) reject assistant messages where content is
// absent — an assistant turn that only made tool calls becomes
// "content": null, never an omitted field. Other roles always emit
// the string form.
func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role       Role            `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		Name       string          `json:"name,omitempty"`
	}
	var content json.RawMessage
	if m.Role == RoleAssistant && len(m.ToolCalls) > 0 && m.Content == "" {
		content = json.RawMessage("null")
	} else {
		b, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		content = b
	}
	return json.Marshal(wire{
		Role: m.Role, Content: content, ToolCalls: m.ToolCalls,
		ToolCallID: m.ToolCallID, Name: m.Name,
	})
}

// ToolCall is a function invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the inner payload of a ToolCall.
// Arguments is a JSON-encoded string as emitted by OpenAI-compatible servers.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec describes a function the model is allowed to call.
// InputSchema is a JSON-Schema object describing the Arguments payload.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Response is the parsed outcome of a single LLM call.
type Response struct {
	Text      string     // assistant's textual reply, if any
	ToolCalls []ToolCall // tool invocations requested by the model
	Finish    string     // e.g. "stop", "tool_calls", "length"
}
