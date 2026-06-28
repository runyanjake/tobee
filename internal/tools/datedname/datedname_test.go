package datedname

import (
	"testing"
	"time"
)

func TestApply(t *testing.T) {
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare name with ext", "notes.md", "2026.04.16-notes.md"},
		{"spaces and case", "My Cool Note.md", "2026.04.16-my-cool-note.md"},
		{"subdir preserved", "notes/things.md", "notes/2026.04.16-things.md"},
		{"nested subdir", "a/b/c/My File.md", "a/b/c/2026.04.16-my-file.md"},
		{"no extension", "scratch", "2026.04.16-scratch"},
		{"underscores collapse", "deploy__notes.md", "2026.04.16-deploy-notes.md"},
		{"punctuation collapse", "v1.2 release!!.md", "2026.04.16-v1-2-release.md"},
		{"uppercase ext", "Plan.MD", "2026.04.16-plan.md"},
		{"trim leading slash", "/notes.md", "2026.04.16-notes.md"},
		{"trim spaces", "  notes.md  ", "2026.04.16-notes.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Apply(tc.in, now)
			if err != nil {
				t.Fatalf("Apply(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Apply(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct{ name, in string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"only slash", "/"},
		{"unusable basename", "!!!.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Apply(tc.in, now); err == nil {
				t.Fatalf("Apply(%q): expected error, got nil", tc.in)
			}
		})
	}
}
