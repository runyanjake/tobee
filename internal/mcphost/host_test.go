package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/scope"
)

// testServer builds a built-in server exercising every path the agent
// depends on: plain output, errors, verbatim, await, and scope.
func testServer() *mcpserver.Server {
	s := mcpserver.New("probe", "Probe instructions.")
	s.Add(mcpserver.Tool{Name: "echo", Handler: func(_ context.Context, args json.RawMessage) (string, error) {
		return string(args), nil
	}})
	s.Add(mcpserver.Tool{Name: "fail", Handler: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("boom")
	}})
	s.Add(mcpserver.Tool{Name: "panic", Handler: func(context.Context, json.RawMessage) (string, error) {
		panic("kaboom")
	}})
	s.Add(mcpserver.Tool{Name: "report", Verbatim: true, Handler: func(context.Context, json.RawMessage) (string, error) {
		return "all quiet", nil
	}})
	s.Add(mcpserver.Tool{Name: "ask", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
		mcpserver.SetResultMeta(ctx, mcpserver.MetaAwait, mcpserver.Await{Question: "which?", Keys: []string{"k1"}})
		return "asked", nil
	}})
	s.Add(mcpserver.Tool{Name: "whoami", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
		sc, ok := scope.From(ctx)
		if !ok {
			return "nobody", nil
		}
		return sc.Connector + "/" + sc.User, nil
	}})
	return s
}

func connected(t *testing.T) *Host {
	t.Helper()
	h := New()
	if err := h.ConnectInProcess(context.Background(), testServer()); err != nil {
		t.Fatalf("ConnectInProcess: %v", err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestCatalogIsNamespaced(t *testing.T) {
	h := connected(t)
	want := []string{"probe_ask", "probe_echo", "probe_fail", "probe_panic", "probe_report", "probe_whoami"}
	got := h.ToolNames()
	if len(got) != len(want) {
		t.Fatalf("ToolNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ToolNames() = %v, want %v", got, want)
		}
	}
	servers := h.Servers()
	if len(servers) != 1 || servers[0].Instructions != "Probe instructions." || !servers[0].Trusted {
		t.Fatalf("Servers() = %+v", servers)
	}
}

func TestCallResults(t *testing.T) {
	h := connected(t)
	ctx := scope.With(context.Background(), scope.UserScope{Connector: "discord", User: "42", Channel: "c"})

	cases := []struct {
		tool  string
		check func(Result) bool
	}{
		{"probe_echo", func(r Result) bool { return r.Text == `{"x":1}` && !r.IsError }},
		{"probe_fail", func(r Result) bool { return r.IsError && r.Text == "boom" }},
		{"probe_panic", func(r Result) bool { return r.IsError }},
		{"probe_report", func(r Result) bool { return r.Verbatim && r.Text == "all quiet" }},
		{"probe_ask", func(r Result) bool { return r.Await != nil && r.Await.Question == "which?" }},
		{"probe_whoami", func(r Result) bool { return r.Text == "discord/42" }},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			res, err := h.Call(ctx, tc.tool, json.RawMessage(`{"x":1}`))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !tc.check(res) {
				t.Fatalf("unexpected result: %+v", res)
			}
		})
	}

	if _, err := h.Call(ctx, "probe_missing", nil); err == nil {
		t.Fatal("unknown tool did not error")
	}
}

// An untrusted server gets no scope and cannot claim verbatim output or
// pause the task (D-038).
func TestUntrustedServerIsSandboxed(t *testing.T) {
	h := New()
	t.Cleanup(h.Close)
	s := testServer()
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	if err := h.Connect(context.Background(), "probe", clientT, Options{}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx := scope.With(context.Background(), scope.UserScope{Connector: "discord", User: "42"})

	if res, _ := h.Call(ctx, "probe_whoami", nil); res.Text != "nobody" {
		t.Fatalf("scope leaked to untrusted server: %q", res.Text)
	}
	if res, _ := h.Call(ctx, "probe_report", nil); res.Verbatim {
		t.Fatal("untrusted verbatim honored")
	}
	if res, _ := h.Call(ctx, "probe_ask", nil); res.Await != nil {
		t.Fatal("untrusted await honored")
	}
	if info := h.Servers(); info[0].Instructions != "" {
		t.Fatalf("untrusted instructions exposed: %q", info[0].Instructions)
	}
}

func TestDisconnectDropsTools(t *testing.T) {
	h := connected(t)
	if err := h.Disconnect("probe"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if n := len(h.ToolNames()); n != 0 {
		t.Fatalf("%d tools left after disconnect", n)
	}
}
