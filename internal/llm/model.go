// Package llm is the agent's only interface to a model: every call is a Decide that
// chooses exactly one of the offered tools (D-041). Providers live in subpackages.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

type Model interface {
	// Decide wraps ErrInvalidDecision when the output is not a valid choice (retryable).
	Decide(ctx context.Context, msgs []Message, tools []ToolSpec) (*Decision, error)
}

// With constrained decoding, ErrInvalidDecision means the server ignored the output schema.
var ErrInvalidDecision = errors.New("model output is not a valid tool choice")

type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type Decision struct {
	// Call has a fresh ID, ready to append as an assistant turn.
	Call ToolCall
	// Reasoning is logged, never sent back to the model.
	Reasoning string
	Raw       string
	Finish    string
	Usage     Usage
}

// Usage is zero when the server does not report it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
