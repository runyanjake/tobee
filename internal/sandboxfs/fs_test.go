package sandboxfs

import (
	"strings"
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

// Blocks are runs of non-blank lines, so coarse search works on any format,
// not just markdown (D-056).
func TestSearchFilesReturnsBlocksForAnyFormat(t *testing.T) {
	fs := testFS(t, map[string]string{
		"notes.md":  "# Food\n\nI like pizza a lot\nwith basil\n\nUnrelated paragraph\n\npizza again here\n",
		"conf.ini":  "[a]\nkey=1\n\n[pizza]\ntoppings=basil\n",
		"other.txt": "nothing to see",
	})

	got, err := fs.SearchFiles("pizza", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("SearchFiles() = %+v, want 2 files", got)
	}
	// Most matches first.
	if got[0].Path != "notes.md" || got[0].Count != 2 {
		t.Fatalf("first file = %+v, want notes.md with 2 matches", got[0])
	}
	// The two hits are in separate paragraphs, so two ranges, not one span.
	if len(got[0].Blocks) != 2 {
		t.Fatalf("blocks = %v, want one per paragraph", got[0].Blocks)
	}
	if got[0].Blocks[0].String() != "L3-4" {
		t.Fatalf("first block = %s, want the paragraph L3-4", got[0].Blocks[0])
	}
	// A non-markdown file gets the same treatment.
	if got[1].Path != "conf.ini" || got[1].Blocks[0].String() != "L4-5" {
		t.Fatalf("conf.ini = %+v, want the [pizza] stanza", got[1])
	}
}

// Adjacent hits collapse into one span rather than many overlapping ones.
func TestSearchFilesMergesAdjacentBlocks(t *testing.T) {
	fs := testFS(t, map[string]string{"list.txt": "pizza one\npizza two\npizza three\n"})
	got, err := fs.SearchFiles("pizza", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Blocks) != 1 || got[0].Blocks[0].String() != "L1-3" {
		t.Fatalf("blocks = %+v, want a single merged L1-3", got)
	}
	if got[0].Count != 3 {
		t.Fatalf("count = %d, want 3", got[0].Count)
	}
}

// One enormous run must not drag in the whole file.
func TestBlockAroundIsClamped(t *testing.T) {
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = "filler"
	}
	lines[250] = "the pizza line"
	got := blockAround(lines, 251)
	if got.To-got.From+1 > maxBlockLines {
		t.Fatalf("block = %s, longer than the %d-line cap", got, maxBlockLines)
	}
	if got.From > 251 || got.To < 251 {
		t.Fatalf("block = %s, does not contain the hit", got)
	}
}

func TestSearchFilesScopedToDir(t *testing.T) {
	fs := testFS(t, map[string]string{
		"a/one.md": "pizza",
		"b/two.md": "pizza",
	})
	got, err := fs.SearchFiles("pizza", "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "a/one.md" {
		t.Fatalf("SearchFiles(dir=a) = %+v, want only a/one.md", got)
	}
}

func TestSliceAndContext(t *testing.T) {
	fs := testFS(t, map[string]string{"doc.txt": "one\ntwo\nthree\nfour\nfive\n"})

	parts, got, err := fs.Slice("doc.txt", []LineRange{{From: 2, To: 3}, {From: 5, To: 99}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0] != "two\nthree" {
		t.Fatalf("Slice() = %q", parts)
	}
	// An over-long range is clamped to the file rather than refused.
	if got[1].To > 6 {
		t.Fatalf("second range = %s, want it clamped to the file", got[1])
	}

	lines, span, err := fs.ContextAround("doc.txt", 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "two|three|four" || span.String() != "L2-4" {
		t.Fatalf("ContextAround() = %q at %s", lines, span)
	}
}
