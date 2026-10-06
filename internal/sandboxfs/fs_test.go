package sandboxfs

import (
	"context"
	"testing"
)

func testFS(t *testing.T, files map[string]string) *FS {
	t.Helper()
	fs, err := NewFS(t.TempDir(), 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range files {
		if err := fs.Write(p, body); err != nil {
			t.Fatal(err)
		}
	}
	return fs
}

// resolve is the security boundary (D-003): nothing may address outside Root.
func TestResolveRejectsEscapes(t *testing.T) {
	fs := testFS(t, map[string]string{"ok.md": "fine"})
	// A volume-qualified path only looks like one on Windows, so it is not in
	// this list: on Linux and macOS "C:\x" is an ordinary relative name.
	for _, p := range []string{"", "/etc/passwd", "../outside.md", "a/../../outside.md", "../../../tmp/x"} {
		if _, err := fs.Read(p); err == nil {
			t.Errorf("Read(%q) was allowed", p)
		}
		if err := fs.Write(p, "x"); err == nil {
			t.Errorf("Write(%q) was allowed", p)
		}
	}
	if got, err := fs.Read("ok.md"); err != nil || got != "fine" {
		t.Fatalf("Read(ok.md) = %q, %v", got, err)
	}
}

// Searching is grep's job; this package only confines it (D-061).
func TestGrepModes(t *testing.T) {
	if err := CheckGrep(); err != nil {
		t.Skipf("no grep: %v", err)
	}
	fs := testFS(t, map[string]string{
		"shopping_list.md":          "- Bread\n- Milk\n- Lettuce\n",
		".tobee/conversations/t.md": "called memory_list shopping_list\nresult shopping_list\n",
		"notes/pizza.txt":           "a\nb\nI like pizza\nc\nd\n",
	})
	ctx := context.Background()

	hits, err := fs.Grep(ctx, GrepOptions{Pattern: "pizza"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "notes/pizza.txt" || hits[0].Line != 3 {
		t.Fatalf("line mode = %+v", hits)
	}

	hits, err = fs.Grep(ctx, GrepOptions{Pattern: "pizza", Context: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || !hits[0].Context || hits[1].Context || !hits[2].Context {
		t.Fatalf("context mode = %+v, want one match between two context lines", hits)
	}

	// Counting mode must not report the files that matched nothing: grep -rc
	// prints a zero for every file it looked at.
	hits, err = fs.Grep(ctx, GrepOptions{Pattern: "shopping", Count: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Count == 0 {
			t.Fatalf("counting mode kept a zero: %+v", hits)
		}
	}

	// No matches is an answer, not an error (grep exits 1).
	hits, err = fs.Grep(ctx, GrepOptions{Pattern: "nothing like this"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty result = %+v, %v", hits, err)
	}
}

// "shopping list" has to find shopping_list, in contents and in names (D-061).
func TestGrepLooseAndNames(t *testing.T) {
	if err := CheckGrep(); err != nil {
		t.Skipf("no grep: %v", err)
	}
	fs := testFS(t, map[string]string{
		"shopping_list.md":   "- Bread\n",
		"notes/mentions.md":  "see the shopping-list for details\n",
		"notes/unrelated.md": "nothing\n",
	})

	hits, err := fs.Grep(context.Background(), GrepOptions{Pattern: "shopping list", Loose: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "notes/mentions.md" {
		t.Fatalf("loose grep = %+v, want the hyphenated mention", hits)
	}

	names, err := fs.MatchNames("Shopping List", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "shopping_list.md" {
		t.Fatalf("MatchNames = %v, want shopping_list.md", names)
	}
	if got, _ := fs.MatchNames("unrelated things", "", 10); len(got) != 0 {
		t.Fatalf("MatchNames matched too much: %v", got)
	}
}

// The model supplies typed options, never arguments: a pattern that looks like
// a flag or a path is still only a pattern (D-061).
func TestGrepCannotSmuggleFlagsOrEscape(t *testing.T) {
	if err := CheckGrep(); err != nil {
		t.Skipf("no grep: %v", err)
	}
	fs := testFS(t, map[string]string{"in.md": "inside the sandbox\n"})
	ctx := context.Background()

	// A flag-shaped pattern is matched literally, not interpreted.
	if hits, err := fs.Grep(ctx, GrepOptions{Pattern: "-r /etc/passwd"}); err != nil || len(hits) != 0 {
		t.Fatalf("flag-shaped pattern = %+v, %v", hits, err)
	}
	// Dir goes through resolve, so it cannot leave the root.
	for _, dir := range []string{"..", "../..", "/etc", "a/../../.."} {
		if _, err := fs.Grep(ctx, GrepOptions{Pattern: "root", Dir: dir}); err == nil {
			t.Fatalf("Grep escaped the sandbox with dir=%q", dir)
		}
	}
	// The argv is ours: only the pattern and "." reach grep as operands.
	args, err := grepArgs(GrepOptions{Pattern: "x", Context: 99}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if args[len(args)-1] != "." || args[len(args)-2] != "x" || args[len(args)-3] != "--" {
		t.Fatalf("argv tail = %v, want -- pattern .", args)
	}
	for i, a := range args {
		if a == "-C" && args[i+1] != "10" {
			t.Fatalf("context was not clamped: %v", args)
		}
	}
}

func TestLoosePattern(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"shopping list", `shopping[-_[:space:]]*list`},
		{"shopping_list", `shopping[-_[:space:]]*list`},
		{"a.b", `a\.b`}, // regex metacharacters in the words stay literal
	} {
		if got := loosePattern(tc.in); got != tc.want {
			t.Errorf("loosePattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
