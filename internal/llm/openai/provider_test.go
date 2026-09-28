package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runyanjake/tobee/internal/llm"
)

var tools = []llm.ToolSpec{
	{Name: "memory_read", Description: "Read a memory file.", InputSchema: json.RawMessage(`{
		"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema",
		"properties":{"path":{"type":"string","description":"File path.","pattern":"^[a-z]"},
		              "scope":{"type":"string","enum":["user","shared"]}},
		"required":["path"]}`)},
	{Name: "step_finish", InputSchema: json.RawMessage(`{"type":"object","properties":{"result":{"type":"string"}},"required":["result"]}`)},
}

func server(t *testing.T, body string) (*Provider, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Options{BaseURL: srv.URL, Model: "m"}), &got
}

func content(s string) string {
	b, _ := json.Marshal(s)
	return `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + string(b) +
		`,"reasoning":"think"}}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`
}

func TestDecideUsesStructuredOutput(t *testing.T) {
	p, got := server(t, content(`{"call":{"tool":"memory_read","arguments":{"path":"INDEX.md"}}}`))

	d, err := p.Decide(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, tools)
	if err != nil {
		t.Fatal(err)
	}
	if d.Call.Function.Name != "memory_read" || d.Call.Function.Arguments != `{"path":"INDEX.md"}` || d.Call.ID == "" {
		t.Fatalf("call = %+v", d.Call)
	}
	if d.Reasoning != "think" || d.Usage.PromptTokens != 10 {
		t.Fatalf("decision = %+v", d)
	}

	req := *got
	if _, ok := req["tools"]; ok {
		t.Fatal("native tools were sent")
	}
	if _, ok := req["tool_choice"]; ok {
		t.Fatal("tool_choice was sent")
	}
	rf := req["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v", rf)
	}
	schema, _ := json.Marshal(rf["json_schema"].(map[string]any)["schema"])
	for _, want := range []string{`"const":"memory_read"`, `"const":"step_finish"`, `"anyOf"`} {
		if !strings.Contains(string(schema), want) {
			t.Fatalf("schema missing %s: %s", want, schema)
		}
	}
	for _, banned := range []string{`$schema`, `pattern`, `description`} {
		if strings.Contains(string(schema), banned) {
			t.Fatalf("schema kept %s: %s", banned, schema)
		}
	}

	// The menu is the last message and is not part of the caller's slice.
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	for _, want := range []string{"<tools>", "memory_read: Read a memory file.", "path (string, required): File path.", `scope ("user" | "shared")`} {
		if !strings.Contains(last, want) {
			t.Fatalf("menu missing %q:\n%s", want, last)
		}
	}
}

func TestDecideRejectsInvalidOutput(t *testing.T) {
	cases := map[string]string{
		"prose":           content(`memory_read({path: "user.md"})`),
		"unoffered tool":  content(`{"call":{"tool":"rm_rf","arguments":{}}}`),
		"arguments array": content(`{"call":{"tool":"memory_read","arguments":[]}}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := server(t, body)
			d, err := p.Decide(context.Background(), nil, tools)
			if !errors.Is(err, llm.ErrInvalidDecision) {
				t.Fatalf("err = %v, want ErrInvalidDecision", err)
			}
			if d == nil || d.Raw == "" {
				t.Fatal("raw output not returned for logging")
			}
		})
	}
}

func TestDecideAcceptsNativeToolCall(t *testing.T) {
	p, _ := server(t, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"step_finish","arguments":"{\"result\":\"done\"}"}}]}}]}`)
	d, err := p.Decide(context.Background(), nil, tools)
	if err != nil || d.Call.Function.Name != "step_finish" || d.Call.ID != "c1" {
		t.Fatalf("d = %+v, err = %v", d, err)
	}
}

// Grammar converters emit properties in schema order; "tool" must come first.
func TestSchemaOrdersToolBeforeArguments(t *testing.T) {
	schema, err := decisionSchema(tools)
	if err != nil {
		t.Fatal(err)
	}
	s := string(schema)
	for _, name := range []string{"memory_read", "step_finish"} {
		if !strings.Contains(s, `"properties":{"tool":{"const":"`+name+`"},"arguments":`) {
			t.Fatalf("%s branch not ordered tool → arguments: %s", name, s)
		}
	}
}

func TestSingleToolSchemaHasNoAnyOf(t *testing.T) {
	schema, err := decisionSchema(tools[1:])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schema), "anyOf") {
		t.Fatalf("single-tool schema should not branch: %s", schema)
	}
}
