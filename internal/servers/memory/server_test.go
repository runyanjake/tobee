package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// Linking accounts to a person moves the old per-account folder, so
// nothing already remembered is stranded (D-045).
func TestLinkIdentitiesMovesLegacyFolder(t *testing.T) {
	fs, _ := sandboxfs.NewFS(t.TempDir(), 64*1024)
	_ = fs.Write("users/discord/123/INDEX.md", "old index")
	LinkIdentities(fs, map[string][]string{"jake": {"discord:123", "email:jake@example.com"}})
	if got, _ := fs.Read("users/jake/INDEX.md"); got != "old index" {
		t.Fatalf("not moved: %q", got)
	}
	// Running again is a no-op.
	LinkIdentities(fs, map[string][]string{"jake": {"discord:123"}})
	if got, _ := fs.Read("users/jake/INDEX.md"); got != "old index" {
		t.Fatal("second run changed the folder")
	}
}

func TestArchiveTranscriptLandsInConversations(t *testing.T) {
	fs, _ := sandboxfs.NewFS(t.TempDir(), 64*1024)
	started := time.Date(2026, 9, 28, 23, 45, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := ArchiveTranscript(fs, "jake", started, "transcript"); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"users/jake/conversations/2026/09/28-2345.md", "users/jake/conversations/2026/09/28-2345-2.md"} {
		if !fs.Exists(p) {
			t.Fatalf("missing %s", p)
		}
	}
}

// Lessons are the one pinned memory file (D-052): bounded, per user, and
// framed so the model can't read its own past guesses as facts.
func TestLessonsArePinnedPerUserAndCapped(t *testing.T) {
	h, fs, ctx := setup(t)

	// Nothing pinned before anything has been learned.
	pinned, err := h.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pinned {
		if strings.Contains(p.Text, "<lessons>") {
			t.Fatalf("an empty lessons file was pinned: %q", p.Text)
		}
	}

	now := time.Date(2026, 9, 29, 16, 0, 0, 0, time.UTC)
	if err := AppendLessons(fs, "discord:me", []string{"list the schedule for the id, then cancel it"}, now); err != nil {
		t.Fatal(err)
	}

	pinned, err = h.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var block string
	for _, p := range pinned {
		if strings.Contains(p.Text, "<lessons>") {
			block = p.Text
		}
	}
	if block == "" {
		t.Fatalf("lessons were not pinned: %+v", pinned)
	}
	for _, want := range []string{"Guidance, not fact", "- 2026-09-29 list the schedule for the id, then cancel it"} {
		if !strings.Contains(block, want) {
			t.Fatalf("pinned block missing %q:\n%s", want, block)
		}
	}

	// Another user's turn never sees them.
	other := scope.With(context.Background(), scope.UserScope{Connector: "discord", User: "other", Channel: "c"})
	pinned, _ = h.Pinned(other)
	for _, p := range pinned {
		if strings.Contains(p.Text, "cancel it") {
			t.Fatalf("one person's lessons reached another: %q", p.Text)
		}
	}

	// A turn with no user at all (a timer) reads nothing rather than failing.
	if pinned, err = h.Pinned(context.Background()); err != nil {
		t.Fatalf("Pinned() with no scope = %v, want no error", err)
	}
	for _, p := range pinned {
		if strings.Contains(p.Text, "<lessons>") {
			t.Fatalf("lessons pinned with no user: %q", p.Text)
		}
	}
}

// The cap is what makes pinning safe: the file keeps the newest lines only.
func TestAppendLessonsTrimsAndBounds(t *testing.T) {
	_, fs, _ := setup(t)
	now := time.Now()

	for i := 0; i < 40; i++ {
		if err := AppendLessons(fs, "discord:me", []string{strings.Repeat("x", 120)}, now); err != nil {
			t.Fatal(err)
		}
	}
	body, err := fs.Read("users/discord/me/" + lessonsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > lessonsMaxBytes {
		t.Fatalf("lessons file is %d bytes, cap is %d", len(body), lessonsMaxBytes)
	}

	// At most three lessons per session, each line bounded.
	_ = fs.Write("users/discord/me/"+lessonsPath, "")
	if err := AppendLessons(fs, "discord:me",
		[]string{"one", "two", "three", "four"}, now); err != nil {
		t.Fatal(err)
	}
	body, _ = fs.Read("users/discord/me/" + lessonsPath)
	if strings.Contains(body, "four") {
		t.Fatalf("more than %d lessons kept:\n%s", lessonsMaxPerRun, body)
	}

	if err := AppendLessons(fs, "discord:me", []string{strings.Repeat("y", 400)}, now); err != nil {
		t.Fatal(err)
	}
	body, _ = fs.Read("users/discord/me/" + lessonsPath)
	for _, line := range strings.Split(body, "\n") {
		if len(line) > lessonLineMax+len("- 2026-09-29 ")+4 {
			t.Fatalf("line of %d bytes exceeds the per-line cap: %q", len(line), line)
		}
	}
}

func TestAppendLessonsIgnoresNothingToSay(t *testing.T) {
	_, fs, _ := setup(t)
	if err := AppendLessons(fs, "discord:me", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if fs.Exists("users/discord/me/" + lessonsPath) {
		t.Fatal("an empty lesson list created the file")
	}
}

// The model wrote shopping_list/INDEX.md while shopping_list.md existed,
// leaving the user with two lists and no error (D-054).
func TestWriteRefusesASecondHomeForTheSameThing(t *testing.T) {
	h, fs, ctx := setup(t)
	_ = fs.Write("users/discord/me/shopping_list.md", "- eggs")

	res, err := h.Call(ctx, "memory_write",
		json.RawMessage(`{"path":"shopping_list/INDEX.md","content":"- milk"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "memory://user/shopping_list.md") {
		t.Fatalf("res = %+v, want a refusal naming the existing file", res)
	}
	if fs.Exists("users/discord/me/shopping_list/INDEX.md") {
		t.Fatal("the duplicate file was written anyway")
	}

	// Writing to the file that already holds it works.
	res, err = h.Call(ctx, "memory_write",
		json.RawMessage(`{"path":"shopping_list.md","content":"- eggs\n- milk"}`))
	if err != nil || res.IsError {
		t.Fatalf("res = %+v, err = %v, want the existing path to be writable", res, err)
	}
}

func TestWriteClashDetection(t *testing.T) {
	_, fs, _ := setup(t)
	root := scopedRoot{Label: "user", Dir: "users/discord/me"}
	_ = fs.Write("users/discord/me/shopping_list.md", "x")
	_ = fs.Write("users/discord/me/topics/food.md", "x")

	tests := []struct {
		name, write, want string
	}{
		{"a directory over an existing file", "shopping_list/INDEX.md", "shopping_list.md"},
		{"a different separator", "shopping-list.md", "shopping_list.md"},
		{"a different extension", "shopping_list.txt", "shopping_list.md"},
		{"overwriting the same path", "shopping_list.md", ""},
		{"a genuinely new file", "recipes.md", ""},
		{"a new file in an existing directory", "topics/wine.md", ""},
		{"a nested name that collides", "topics/food/notes.md", "topics/food.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clashingPath(fs, root, "users/discord/me/"+tc.write)
			if got != tc.want {
				t.Fatalf("clashingPath(%q) = %q, want %q", tc.write, got, tc.want)
			}
		})
	}
}

// Case is compared in the stem rather than through the filesystem, which is
// case-insensitive on macOS and not on the prod host.
func TestNormalizeStem(t *testing.T) {
	same := []string{"shopping_list.md", "Shopping-List.md", "shopping list.txt", "shoppinglist"}
	want := normalizeStem(same[0])
	for _, p := range same[1:] {
		if got := normalizeStem(p); got != want {
			t.Fatalf("normalizeStem(%q) = %q, want %q", p, got, want)
		}
	}
	if normalizeStem("recipes.md") == want {
		t.Fatal("normalizeStem collapsed two different names")
	}
}

// Knowing what exists shouldn't need a tool call the model won't make (D-054).
func TestManifestIsPinnedWithNamesOnly(t *testing.T) {
	h, fs, ctx := setup(t)
	_ = fs.Write("users/discord/me/shopping_list.md", "- eggs and other secrets")
	_ = fs.Write("users/discord/me/conversations/2026/09/29-0214.md", "a transcript")
	_ = fs.Write("users/discord/me/conversations/2026/09/29-2202.md", "another")

	var block string
	pinned, err := h.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pinned {
		if strings.Contains(p.Text, "<memory-files>") {
			block = p.Text
		}
	}
	if block == "" {
		t.Fatalf("the manifest was not pinned: %+v", pinned)
	}
	for _, want := range []string{"INDEX.md", "shopping_list.md", "conversations/ (2 saved conversations)"} {
		if !strings.Contains(block, want) {
			t.Fatalf("manifest missing %q:\n%s", want, block)
		}
	}
	// Names only: no contents, and no transcript paths crowding it out.
	if strings.Contains(block, "secrets") || strings.Contains(block, "29-0214") {
		t.Fatalf("manifest leaked contents or transcript paths:\n%s", block)
	}
	if strings.Contains(block, lessonsPath) {
		t.Fatalf("lessons are pinned in full already:\n%s", block)
	}
}

func TestManifestEmptyWithoutAUser(t *testing.T) {
	h, _, _ := setup(t)
	pinned, err := h.Pinned(context.Background())
	if err != nil {
		t.Fatalf("Pinned() = %v, want no error with no scope", err)
	}
	for _, p := range pinned {
		if strings.Contains(p.Text, "<memory-files>") {
			t.Fatalf("a manifest was pinned with no user: %q", p.Text)
		}
	}
}
