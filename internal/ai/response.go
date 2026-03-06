package ai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCall represents a single action invocation requested by the AI.
// Using a structured array (rather than a plain string) allows the caller
// to dispatch multiple tools in one response and to inspect each call
// independently — important for agentic loops and error reporting.
type ToolCall struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// AIResponse is the structured output the AI is instructed to return.
type AIResponse struct {
	Response  string     `json:"response"`
	ToolCalls []ToolCall `json:"tool_calls"`
}

// ParseResponse extracts an AIResponse from a raw AI output string.
// It tolerates JSON wrapped in markdown code fences, which many models
// produce even when instructed not to.
func ParseResponse(raw string) (*AIResponse, error) {
	cleaned := cleanRaw(raw)
	var resp AIResponse
	if err := json.Unmarshal([]byte(cleaned), &resp); err != nil {
		return nil, fmt.Errorf("parsing AI response: %w (raw: %q)", err, raw)
	}
	return &resp, nil
}

// cleanRaw strips reasoning model artifacts and markdown fences from a raw
// AI output string so it can be parsed as JSON.
//
// Reasoning models (e.g. DeepSeek-R1, QwQ) wrap their internal monologue in
// <think>...</think> tags and emit the actual response after the closing tag.
// Markdown fences (```json ... ```) are also removed.
func cleanRaw(s string) string {
	s = strings.TrimSpace(s)

	// Strip <think>...</think> block — take everything after </think>
	if idx := strings.LastIndex(s, "</think>"); idx != -1 {
		s = strings.TrimSpace(s[idx+len("</think>"):])
	}

	// Strip markdown code fences
	if strings.HasPrefix(s, "```") {
		newline := strings.Index(s, "\n")
		if newline == -1 {
			return s
		}
		s = s[newline+1:]
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	}

	return strings.TrimSpace(s)
}
