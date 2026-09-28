package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/sandboxfs"
	"github.com/runyanjake/tobee/internal/scope"
)

func setup(t *testing.T) (*mcphost.Host, *sandboxfs.FS, context.Context) {
	t.Helper()
	fs, err := sandboxfs.NewFS(t.TempDir(), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	_ = fs.Write("users/discord/me/INDEX.md", "my index")
	_ = fs.Write("users/discord/other/secret.md", "not yours")
	_ = fs.Write("shared/house.md", "wifi is tobeenet")

	h := mcphost.New()
	t.Cleanup(h.Close)
	if err := h.ConnectInProcess(context.Background(), New("", fs)); err != nil {
		t.Fatal(err)
	}
	ctx := scope.With(context.Background(), scope.UserScope{Connector: "discord", User: "me", Channel: "c"})
	return h, fs, ctx
}

func TestReadResourcesByScope(t *testing.T) {
	h, _, ctx := setup(t)
	for uri, want := range map[string]string{
		"memory://user/INDEX.md":   "my index",
		"memory://shared/house.md": "wifi is tobeenet",
	} {
		got, err := h.ReadResource(ctx, uri)
		if err != nil || got != want {
			t.Fatalf("ReadResource(%s) = %q, %v; want %q", uri, got, err, want)
		}
	}
}

// Nothing addressable from one user's turn may reach another user's tree
// (D-013). sandboxfs alone only confines paths to the whole memory root.
func TestScopeCannotBeEscaped(t *testing.T) {
	h, fs, ctx := setup(t)

	for _, uri := range []string{
		"memory://user/../other/secret.md",
		"memory://user/../../discord/other/secret.md",
		"memory://shared/../users/discord/other/secret.md",
		"memory://users/discord/other/secret.md",
	} {
		if got, err := h.ReadResource(ctx, uri); err == nil {
			t.Errorf("ReadResource(%s) = %q, want an error", uri, got)
		}
	}

	for tool, args := range map[string]string{
		"memory_write":  `{"path":"../../discord/other/secret.md","content":"pwned"}`,
		"memory_append": `{"path":"../other/secret.md","content":"pwned"}`,
		"memory_list":   `{"dir":"../other"}`,
	} {
		res, err := h.Call(ctx, tool, json.RawMessage(args))
		if err != nil || !res.IsError {
			t.Errorf("%s escaped its scope: %+v, %v", tool, res, err)
		}
	}
	if got, _ := fs.Read("users/discord/other/secret.md"); got != "not yours" {
		t.Fatalf("other user's file changed: %q", got)
	}
}

func TestListAndSearchReturnURIs(t *testing.T) {
	h, _, ctx := setup(t)
	res, err := h.Call(ctx, "memory_list", nil)
	if err != nil || !strings.Contains(res.Text, "memory://user/INDEX.md") || !strings.Contains(res.Text, "memory://shared/house.md") {
		t.Fatalf("list = %q, %v", res.Text, err)
	}
	if strings.Contains(res.Text, "secret") {
		t.Fatalf("list leaked another user's file: %q", res.Text)
	}
	res, _ = h.Call(ctx, "memory_search", json.RawMessage(`{"query":"wifi"}`))
	if !strings.HasPrefix(res.Text, "memory://shared/house.md:1") {
		t.Fatalf("search = %q", res.Text)
	}
}

// Without a user, user-scoped URIs fail rather than resolve somewhere generic.
func TestUserScopeNeedsAUser(t *testing.T) {
	h, _, _ := setup(t)
	ctx := scope.With(context.Background(), scope.UserScope{Connector: "mcp_feed", Channel: "c"})
	if _, err := h.ReadResource(ctx, "memory://user/INDEX.md"); err == nil {
		t.Fatal("user scope resolved with no user attached")
	}
}
