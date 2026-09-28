// Package openai forces one tool choice via a response_format JSON Schema, not native
// tools/tool_choice, which Ollama ignores (D-041).
package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
)

// Low because every call is a structured decision; the voice comes from the persona.
const DefaultTemperature = 0.1

// Zero values mean defaults; Temperature is a pointer so 0 is expressible.
type Options struct {
	BaseURL     string // /v1/chat/completions is appended
	Model       string
	APIKey      string // sent as a bearer token when set
	Temperature *float64
	MaxTokens   int           // default 2048
	Timeout     time.Duration // HTTP timeout, default 10m
}

type Provider struct {
	baseURL     string
	model       string
	apiKey      string
	temperature float64
	maxTokens   int
	http        *http.Client
}

var _ llm.Model = (*Provider)(nil)

func New(o Options) *Provider {
	p := &Provider{
		baseURL:     o.BaseURL,
		model:       o.Model,
		apiKey:      o.APIKey,
		temperature: DefaultTemperature,
		maxTokens:   o.MaxTokens,
		http:        &http.Client{Timeout: o.Timeout},
	}
	if o.Temperature != nil {
		p.temperature = *o.Temperature
	}
	if p.maxTokens <= 0 {
		p.maxTokens = 2048
	}
	if p.http.Timeout <= 0 {
		p.http.Timeout = 10 * time.Minute
	}
	return p
}

func (p *Provider) Decide(ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec) (*llm.Decision, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("openai: decide needs at least one tool")
	}
	schema, err := decisionSchema(tools)
	if err != nil {
		return nil, err
	}

	wire := make([]llm.Message, 0, len(msgs)+1)
	wire = append(wire, msgs...)
	// The schema isn't shown to the model, so each request ends with a tool menu, never stored.
	wire = append(wire, llm.Message{Role: llm.RoleUser, Content: renderMenu(tools)})

	req := chatRequest{
		Model:       p.model,
		Messages:    wire,
		Temperature: p.temperature,
		MaxTokens:   p.maxTokens,
		ResponseFormat: &responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchema{
				Name:   "tool_call",
				Schema: schema,
				// Strict mode on hosted APIs forbids optional properties, which tool schemas are full of.
				Strict: false,
			},
		},
	}
	cr, err := p.post(ctx, req)
	if err != nil {
		return nil, err
	}

	ch := cr.Choices[0]
	d := &llm.Decision{
		Reasoning: ch.Message.ReasoningContent,
		Raw:       ch.Message.Content,
		Finish:    ch.FinishReason,
		Usage:     cr.Usage,
	}
	if d.Reasoning == "" {
		d.Reasoning = ch.Message.Reasoning
	}
	call, err := parseDecision(ch.Message, tools)
	if err != nil {
		return d, err
	}
	d.Call = call
	return d, nil
}

// A native tool call is accepted too: still a structured choice, not salvaged text (D-025).
func parseDecision(m responseMessage, tools []llm.ToolSpec) (llm.ToolCall, error) {
	offered := make(map[string]bool, len(tools))
	for _, t := range tools {
		offered[t.Name] = true
	}

	if len(m.ToolCalls) > 0 {
		tc := m.ToolCalls[0]
		if !offered[tc.Function.Name] {
			return llm.ToolCall{}, fmt.Errorf("%w: native call to unoffered tool %q", llm.ErrInvalidDecision, tc.Function.Name)
		}
		if tc.ID == "" {
			tc.ID = newCallID()
		}
		tc.Type = "function"
		return tc, nil
	}

	var out struct {
		Call struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"call"`
	}
	if err := json.Unmarshal([]byte(m.Content), &out); err != nil {
		return llm.ToolCall{}, fmt.Errorf("%w: %v", llm.ErrInvalidDecision, err)
	}
	if !offered[out.Call.Tool] {
		return llm.ToolCall{}, fmt.Errorf("%w: unoffered tool %q", llm.ErrInvalidDecision, out.Call.Tool)
	}
	args := bytes.TrimSpace(out.Call.Arguments)
	if len(args) == 0 || args[0] != '{' {
		return llm.ToolCall{}, fmt.Errorf("%w: arguments for %s are not an object", llm.ErrInvalidDecision, out.Call.Tool)
	}
	return llm.ToolCall{
		ID:       newCallID(),
		Type:     "function",
		Function: llm.FunctionCall{Name: out.Call.Tool, Arguments: string(args)},
	}, nil
}

func (p *Provider) post(ctx context.Context, req chatRequest) (*chatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai: status %d: %s", resp.StatusCode, string(b))
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices in response")
	}
	return &cr, nil
}

func newCallID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []llm.Message   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	Stream         bool            `json:"stream"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   llm.Usage    `json:"usage"`
}

type chatChoice struct {
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// Reasoning arrives as reasoning_content (vLLM, LM Studio, DeepSeek) or reasoning (Ollama).
type responseMessage struct {
	Content          string         `json:"content"`
	ToolCalls        []llm.ToolCall `json:"tool_calls"`
	ReasoningContent string         `json:"reasoning_content"`
	Reasoning        string         `json:"reasoning"`
}
