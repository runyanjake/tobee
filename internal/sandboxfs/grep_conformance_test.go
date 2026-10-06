package sandboxfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Search runs a binary we don't ship, so its behaviour is part of the
// deployment. Prod is GNU grep inside the Alpine image on an Ubuntu host; dev
// is whatever macOS has (BSD grep, or ugrep/GNU from Homebrew). This runs the
// same battery against every grep it can find, so a variant that differs where
// we depend on it fails here instead of in a turn (D-061).
//
// Point it at a specific set with TOBEE_GREP_BINS, which is how to check the
// deployment target:
//
//	TOBEE_GREP_BINS=/usr/bin/grep go test ./internal/sandboxfs/ -run Conformance -v
func grepCandidates(t *testing.T) []string {
	t.Helper()
	if env := os.Getenv("TOBEE_GREP_BINS"); env != "" {
		var out []string
		for _, b := range strings.FieldsFunc(env, func(r rune) bool { return r == ',' || r == ':' }) {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
		return out
	}
	// Whatever this machine has. /bin/grep is BusyBox on Alpine, which is
	// exactly the variant we want to see fail loudly.
	seen := map[string]bool{}
	var out []string
	for _, b := range []string{"grep", "/usr/bin/grep", "/bin/grep", "ggrep"} {
		path, err := exec.LookPath(b)
		if err != nil || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, b)
	}
	return out
}

func TestGrepConformance(t *testing.T) {
	cands := grepCandidates(t)
	if len(cands) == 0 {
		t.Skip("no grep binary found")
	}
	orig := GrepBin
	t.Cleanup(func() { GrepBin = orig })

	for _, bin := range cands {
		t.Run(bin, func(t *testing.T) {
			GrepBin = bin
			version, err := exec.Command(bin, "--version").Output()
			if err != nil {
				t.Skipf("%s will not run: %v", bin, err)
			}
			first, _, _ := strings.Cut(strings.TrimSpace(string(version)), "\n")
			t.Logf("%s: %s", bin, first)

			// The boot check itself: both output modes, context lines, loose
			// patterns, zero-count rows dropped, and exit 1 not an error.
			// Explicit about TOBEE_GREP_BINS so a required binary can't be
			// quietly skipped when someone asked for it by name.
			if err := grepSelfTest(); err != nil {
				if os.Getenv("TOBEE_GREP_BINS") == "" {
					t.Skipf("%s is not a usable grep (BusyBox lacks -I): %v", bin, err)
				}
				t.Fatalf("%s was named in TOBEE_GREP_BINS but is not usable: %v", bin, err)
			}
			t.Run("confines symlinks", func(t *testing.T) { assertGrepConfines(t) })
			t.Run("trims the path prefix", func(t *testing.T) { assertGrepPaths(t) })
		})
	}
}

// assertGrepConfines is the security-relevant difference between variants: we
// pass -r, never -R, so a symlink found while walking is not followed. A grep
// that treats them alike would read outside the sandbox.
func assertGrepConfines(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "leak.md"), []byte("CANARY out of bounds\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.md"), []byte("CANARY in bounds\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both shapes: a link to the directory, and a link to the file itself.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "leak.md"), filepath.Join(root, "linkfile.md")); err != nil {
		t.Fatal(err)
	}

	fs := &FS{Root: root, MaxFileSize: 1 << 20}
	hits, err := fs.Grep(context.Background(), GrepOptions{Pattern: "CANARY"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits at all; the fixture or the parser is wrong")
	}
	for _, h := range hits {
		if strings.Contains(h.Text, "out of bounds") {
			t.Fatalf("%s followed a symlink out of the sandbox: %s:%d %q",
				GrepBin, h.Path, h.Line, h.Text)
		}
		if strings.HasPrefix(h.Path, "/") || strings.Contains(h.Path, "..") {
			t.Fatalf("%s returned a path outside the scope: %q", GrepBin, h.Path)
		}
	}
}

// assertGrepPaths pins the row shape the renderers rely on: scope-relative,
// no "./" (BSD and GNU print it, ugrep does not), and a real line number.
func assertGrepPaths(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "deep.md"), []byte("x\nneedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &FS{Root: root, MaxFileSize: 1 << 20}
	hits, err := fs.Grep(context.Background(), GrepOptions{Pattern: "needle"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	if got := hits[0].Path; got != "sub/deep.md" {
		t.Fatalf("path = %q, want %q", got, "sub/deep.md")
	}
	if hits[0].Line != 2 {
		t.Fatalf("line = %d, want 2", hits[0].Line)
	}
	if hits[0].Text != "needle" {
		t.Fatalf("text = %q, want %q", hits[0].Text, "needle")
	}
}
