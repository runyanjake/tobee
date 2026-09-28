package llm

import (
	"encoding/json"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message with RoleTool must set ToolCallID; Content carries the tool output.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// MarshalJSON always emits content: LM Studio rejects assistant messages without it,
// so a tool-call-only turn sends "content": null.
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

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// Arguments is a JSON-encoded string, as OpenAI-compatible servers emit it.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
