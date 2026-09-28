package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/scope"
)

func host(t *testing.T) *mcphost.Host {
	t.Helper()
	b, err := New(Config{Token: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h := mcphost.New()
	t.Cleanup(h.Close)
	if err := h.ConnectInProcess(context.Background(), b.Server("")); err != nil {
		t.Fatal(err)
	}
	return h
}

// Sending into the conversation being answered duplicates the reply, which
// is delivered in code (D-035). Refused before any network call.
func TestSendMessageRefusesCurrentConversation(t *testing.T) {
	h := host(t)
	ctx := scope.With(context.Background(), scope.UserScope{Connector: Name, Channel: "123", User: "u"})
	res, err := h.Call(ctx, "discord_send_message", json.RawMessage(`{"channel_id":"123","text":"hi"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "call reply instead") {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestSendMessageIsExternal(t *testing.T) {
	for _, tool := range host(t).Tools() {
		if tool.Name == "discord_send_message" && tool.Category != llm.CategoryExternal {
			t.Fatalf("category = %q", tool.Category)
		}
	}
}
