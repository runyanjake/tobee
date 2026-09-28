package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallParsesReasoningAndUsage(t *testing.T) {
	cases := map[string]string{
		"reasoning_content (vLLM, LM Studio)": `"reasoning_content": "weigh the options"`,
		"reasoning (Ollama)":                  `"reasoning": "weigh the options"`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,` +
					field + `}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`))
			}))
			defer srv.Close()

			resp, err := NewClient(srv.URL, "m", Options{}).Call(context.Background(), nil, nil, ToolChoiceUnset)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Reasoning != "weigh the options" || resp.Text != "" {
				t.Fatalf("resp = %+v", resp)
			}
			if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 3 {
				t.Fatalf("usage = %+v", resp.Usage)
			}
		})
	}
}
