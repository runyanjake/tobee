package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Prompts carry no call syntax and no worked arguments. The rule is old — the
// local model copied `tool({args})` examples as text instead of making a tool
// call, so every example was stripped on 2026-09-28 — but it lived only as
// prose in DESIGN.md, and a prompt rewrite put arguments back. The model then
// copied the example's literal value: an instruction reading `memory_list
// name="Shopping List"` produced a real search for "shopping list" in the
// middle of a conversation about basketball reminders, spending two steps of
// the turn (D-062). Tool names appear bare; what an argument does is described
// in words.
func TestPromptsCarryNoCallSyntaxOrWorkedArguments(t *testing.T) {
	banned := []struct {
		what string
		re   *regexp.Regexp
	}{
		{"a tool call", regexp.MustCompile(`[A-Za-z_]+\(\s*[{"]`)},
		{"an argument assignment", regexp.MustCompile(`[A-Za-z_]{2,}=`)},
	}

	root := filepath.Join("..", "..", "prompts")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("no prompts directory: %v", err)
	}
	var checked int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(body), "\n") {
			for _, b := range banned {
				if m := b.re.FindString(line); m != "" {
					t.Errorf("%s:%d contains %s (%q). Describe what the argument does in words; "+
						"the model copies a literal example as a literal value.\n  %s",
						rel, i+1, b.what, m, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking prompts: %v", err)
	}
	if checked == 0 {
		t.Fatal("no prompt files were checked")
	}
	t.Logf("checked %d prompt files", checked)
}
