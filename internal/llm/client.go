package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a minimal OpenAI-compatible chat client that speaks the
// native tool-calling protocol. Targets LM Studio but works against any
// server implementing /v1/chat/completions with tools.
type Client struct {
	baseURL     string
	model       string
	temperature float64
	maxTokens   int
	http        *http.Client
}

// Options configures a Client. Zero values mean "use sensible defaults".
type Options struct {
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
}

func NewClient(baseURL, model string, opts Options) *Client {
	c := &Client{
		baseURL:     baseURL,
		model:       model,
		temperature: opts.Temperature,
		maxTokens:   opts.MaxTokens,
		http:        &http.Client{Timeout: opts.Timeout},
	}
	if c.temperature == 0 {
		c.temperature = 0.7
	}
	if c.maxTokens == 0 {
		c.maxTokens = 2048
	}
	if c.http.Timeout == 0 {
		c.http.Timeout = 600 * time.Second
	}
	return c
}

// Call sends messages to the LLM. If tools is non-empty it is advertised
// via the OpenAI `tools` field so the model may invoke them. toolChoice
// constrains the model's tool-use behaviour for this call: ToolChoiceUnset
// omits the field (server default), ToolChoiceRequired forces a tool call,
// etc. Required + empty tools is a caller bug and would 400 at the server.
func (c *Client) Call(ctx context.Context, messages []Message, tools []ToolSpec, toolChoice ToolChoice) (*Response, error) {
	req := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
		Stream:      false,
	}
	if len(tools) > 0 {
		req.Tools = make([]toolEnvelope, len(tools))
		for i, t := range tools {
			req.Tools[i] = toolEnvelope{
				Type: "function",
				Function: toolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			}
		}
		if toolChoice != ToolChoiceUnset {
			req.ToolChoice = string(toolChoice)
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("llm status %d: %s", resp.StatusCode, string(b))
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("llm returned no choices")
	}

	ch := cr.Choices[0]
	return &Response{
		Text:      ch.Message.Content,
		ToolCalls: ch.Message.ToolCalls,
		Finish:    ch.FinishReason,
	}, nil
}

// --- wire types ---

type chatRequest struct {
	Model       string         `json:"model"`
	Messages    []Message      `json:"messages"`
	Temperature float64        `json:"temperature"`
	MaxTokens   int            `json:"max_tokens"`
	Stream      bool           `json:"stream"`
	Tools       []toolEnvelope `json:"tools,omitempty"`
	ToolChoice  string         `json:"tool_choice,omitempty"`
}

type toolEnvelope struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []chatChoice   `json:"choices"`
	Usage   map[string]any `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}
