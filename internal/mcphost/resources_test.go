package mcphost

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/mcpserver"
)

func resourceServer(name string, pinned ...string) *mcpserver.Server {
	s := mcpserver.New(name, "")
	for _, uri := range pinned {
		s.AddResource(mcpserver.Resource{URI: uri, Name: uri, Pinned: true,
			Read: func(context.Context) (string, error) { return "pinned " + uri, nil }})
	}
	s.AddResourceTemplate(mcpserver.ResourceTemplate{URITemplate: name + "://{+path}", Name: name,
		Read: func(_ context.Context, uri string) (string, error) { return "read " + uri, nil }})
	return s
}

func TestPinnedInServerThenURIOrder(t *testing.T) {
	h := New()
	defer h.Close()
	_ = h.ConnectInProcess(context.Background(), resourceServer("system", "system://prompt/01-b.md", "system://prompt/00-a.md"))

	got, err := h.Pinned(context.Background())
	if err != nil || len(got) != 2 || got[0].URI != "system://prompt/00-a.md" || got[1].Text != "pinned system://prompt/01-b.md" {
		t.Fatalf("Pinned() = %+v, %v", got, err)
	}
}

// Pinned text lands in the system prompt; an untrusted server must not
// be able to put it there (D-038).
func TestUntrustedServerCannotPin(t *testing.T) {
	h := New()
	defer h.Close()
	clientT, serverT := mcp.NewInMemoryTransports()
	s := resourceServer("ext", "ext://inject.md")
	if _, err := s.MCP().Connect(context.Background(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Connect(context.Background(), "ext", clientT, Options{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Pinned(context.Background()); len(got) != 0 {
		t.Fatalf("untrusted pinned resources included: %+v", got)
	}
	// Its resources are still readable on request, like any tool result.
	if got, err := h.ReadResource(context.Background(), "ext://inject.md"); err != nil || got != "pinned ext://inject.md" {
		t.Fatalf("ReadResource = %q, %v", got, err)
	}
}

func TestReadRoutesByTemplatePrefix(t *testing.T) {
	h := New()
	defer h.Close()
	_ = h.ConnectInProcess(context.Background(), resourceServer("alpha"))
	_ = h.ConnectInProcess(context.Background(), resourceServer("beta"))

	if got, err := h.ReadResource(context.Background(), "beta://notes/x.md"); err != nil || got != "read beta://notes/x.md" {
		t.Fatalf("ReadResource = %q, %v", got, err)
	}
	if _, err := h.ReadResource(context.Background(), "gamma://x"); err == nil {
		t.Fatal("unowned URI did not error")
	}
	list := h.Resources()
	if len(list) != 2 || !list[0].Template || list[0].Server != "alpha" {
		t.Fatalf("Resources() = %+v", list)
	}
}
