package sandboxfs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Searching is grep's job, not ours. A hand-written scanner had to reinvent
// counts, context windows and name matching, and got the ordering wrong; the
// real tool already has all of it. This runs grep itself, confined to the
// sandbox, with an argv we build from typed options — the model never supplies
// a flag (D-061).
const (
	grepTimeout   = 20 * time.Second
	grepMaxOutput = 512 * 1024 // grep can emit far more than a turn can use
	grepMaxLimit  = 200
)

// GrepBin is the binary to run. Overridable so an operator can pin a path;
// the output parser expects grep's standard formats.
var GrepBin = "grep"

// GrepOptions is the whole surface the model can influence. Everything here is
// typed, so nothing it sends can become an argument of its own.
type GrepOptions struct {
	Pattern string // required; literal unless Regexp
	Dir     string // scope-relative subtree, "" for the whole sandbox
	Regexp  bool   // treat Pattern as an extended regular expression
	Loose   bool   // "shopping list" also matches shopping_list and shopping-list
	Word    bool   // whole words only
	Case    bool   // case-sensitive; the default is insensitive
	Context int    // lines either side of a match
	Count   bool   // per-file match counts instead of lines
	Limit   int    // max rows returned
}

// GrepHit is one row of output. Count is set in counting mode; Line and Text
// in line mode, where Context marks a surrounding line rather than a match.
type GrepHit struct {
	Path    string
	Line    int
	Text    string
	Count   int
	Context bool
}

// Grep runs grep inside the sandbox and returns its rows. A pattern that
// matches nothing is not an error.
func (m *FS) Grep(ctx context.Context, o GrepOptions) ([]GrepHit, error) {
	if strings.TrimSpace(o.Pattern) == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	// The path goes through resolve, which is the security boundary: grep only
	// ever sees a directory we computed (D-003).
	root := m.Root
	if o.Dir != "" {
		abs, err := m.resolve(o.Dir)
		if err != nil {
			return nil, err
		}
		root = abs
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, nil // nothing there yet
	}

	limit := o.Limit
	if limit <= 0 || limit > grepMaxLimit {
		limit = grepMaxLimit
	}
	args, err := grepArgs(o, limit)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, GrepBin, args...)
	// Running in the root means grep prints paths relative to it, so no host
	// path can reach the model, and "." is the only path argument it gets.
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("grep: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("grep: %w (is %q installed?)", err, GrepBin)
	}
	hits := parseGrep(io.LimitReader(stdout, grepMaxOutput), o.Count, limit)
	_, _ = io.Copy(io.Discard, stdout) // drain so grep is never blocked on a full pipe
	if err := cmd.Wait(); err != nil {
		// Exit 1 is grep's "no matches"; anything else is a real failure.
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); !ok || ee.ExitCode() != 1 {
			if ctx.Err() != nil {
				return hits, fmt.Errorf("grep: timed out after %s", grepTimeout)
			}
			return hits, fmt.Errorf("grep: %w", err)
		}
	}
	return hits, nil
}

// grepArgs builds the argv. Only flags named here can ever be passed, and the
// pattern and path are placed after "--" so neither can look like one.
func grepArgs(o GrepOptions, limit int) ([]string, error) {
	args := []string{
		"-r", // recursive, and does not follow symlinks found while walking
		"-s", // stay quiet about unreadable files
		"-I", // skip binaries
	}
	if o.Count {
		args = append(args, "-c")
	} else {
		args = append(args, "-n")
		if o.Context > 0 {
			args = append(args, "-C", strconv.Itoa(min(o.Context, 10)))
		}
		args = append(args, "-m", strconv.Itoa(limit)) // per file, so one file can't fill the answer
	}
	if !o.Case {
		args = append(args, "-i")
	}
	if o.Word {
		args = append(args, "-w")
	}

	pattern := o.Pattern
	switch {
	case o.Loose:
		// Natural language rarely matches a filename or a heading exactly:
		// "shopping list" has to find shopping_list too (D-061).
		pattern = loosePattern(o.Pattern)
		args = append(args, "-E")
	case o.Regexp:
		args = append(args, "-E")
	default:
		args = append(args, "-F")
	}
	return append(args, "--", pattern, "."), nil
}

// loosePattern lets any run of spaces, hyphens and underscores stand for any
// other, so one phrase matches every spelling of it.
func loosePattern(q string) string {
	var parts []string
	for _, w := range strings.FieldsFunc(q, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '-' || r == '_'
	}) {
		parts = append(parts, regexp.QuoteMeta(w))
	}
	if len(parts) == 0 {
		return regexp.QuoteMeta(q)
	}
	return strings.Join(parts, "[-_[:space:]]*")
}

// grepLine matches both of grep's forms: "path:line:text" for a match and
// "path-line-text" for a context line.
var grepLine = regexp.MustCompile(`^(.*?)([:-])([0-9]+)[:-](.*)$`)

func parseGrep(r io.Reader, counting bool, limit int) []GrepHit {
	var out []GrepHit
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() && len(out) < limit {
		line := strings.TrimPrefix(sc.Text(), "./")
		if counting {
			path, n, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			count, err := strconv.Atoi(n)
			if err != nil || count == 0 {
				continue // grep -r -c lists every file, including the ones with none
			}
			out = append(out, GrepHit{Path: path, Count: count})
			continue
		}
		mm := grepLine.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		num, err := strconv.Atoi(mm[3])
		if err != nil {
			continue
		}
		out = append(out, GrepHit{
			Path:    strings.TrimPrefix(mm[1], "./"),
			Line:    num,
			Text:    mm[4],
			Context: mm[2] == "-",
		})
	}
	return out
}

// MatchNames returns the paths whose name matches a phrase, ignoring case,
// separators and spacing, so "Shopping List" finds shopping_list.md. grep
// reads contents; this is the other half, and it is why a file named after a
// topic was unfindable (D-060, D-061).
func (m *FS) MatchNames(phrase, relDir string, limit int) ([]string, error) {
	want := normalizeName(phrase)
	if want == "" {
		return nil, nil
	}
	files, err := m.List(relDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if strings.Contains(normalizeName(f), want) {
			out = append(out, f)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// normalizeName drops case, separators and the extension so every spelling of
// a name compares equal.
func normalizeName(p string) string {
	p = strings.TrimSuffix(p, filepath.Ext(p))
	var b strings.Builder
	for _, r := range strings.ToLower(p) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '/':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// grepOnce logs which grep is in use, once, so a surprising build shows up in
// the boot log rather than in a parsing bug.
var grepOnce sync.Once

// CheckGrep proves the binary works by running a real search with the same
// argv the tools use, not by reading its version. A version string can't tell
// GNU from a variant, and the failure that matters is a grep that runs but
// rejects a flag — BusyBox has no -I, so on Alpine the whole feature turns on
// /usr/bin (GNU, from the grep package) winning over /bin (BusyBox) on PATH.
// Boot is the right place to find that out (D-061).
func CheckGrep() error {
	out, err := exec.Command(GrepBin, "--version").Output()
	if err != nil {
		return fmt.Errorf("grep: %q is not runnable: %w", GrepBin, err)
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")

	if err := grepSelfTest(); err != nil {
		return fmt.Errorf("grep: %q (%s) does not behave as required: %w", GrepBin, version, err)
	}
	grepOnce.Do(func() {
		slog.Info("sandboxfs: grep ready", "bin", GrepBin, "version", version)
	})
	return nil
}

// grepSelfTest searches a throwaway tree, exercising both output modes and the
// flags every call passes, and checks the rows parse back as expected.
func grepSelfTest() error {
	dir, err := os.MkdirTemp("", "tobee-grep-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	// Three files: a two-line match, a nested alternate spelling for the loose
	// pattern, and one with no match so counting mode's zero rows are covered.
	for path, body := range map[string]string{
		"a.md":     "shopping list\nbread\n",
		"sub/b.md": "shopping_list again\n",
		"none.md":  "nothing to find here\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			return fmt.Errorf("temp file: %w", err)
		}
	}
	fs := &FS{Root: dir, MaxFileSize: 1 << 20}
	ctx := context.Background()

	// Line mode with context, loose pattern: both spellings, and the context
	// line that proves "path-line-text" parses as well as "path:line:text".
	hits, err := fs.Grep(ctx, GrepOptions{Pattern: "shopping list", Loose: true, Context: 1})
	if err != nil {
		return fmt.Errorf("line mode: %w", err)
	}
	var matched, ctxLines int
	for _, h := range hits {
		if h.Line == 0 {
			return fmt.Errorf("line mode: row for %q has no line number", h.Path)
		}
		if h.Context {
			ctxLines++
		} else {
			matched++
		}
	}
	if matched != 2 {
		return fmt.Errorf("line mode: matched %d files, want 2 (is -E or -i unsupported?)", matched)
	}
	if ctxLines == 0 {
		return fmt.Errorf("line mode: -C returned no surrounding lines")
	}

	// Counting mode: two files with matches, and the file with none must not
	// appear — grep -r -c lists it as ":0" and the parser drops it.
	counts, err := fs.Grep(ctx, GrepOptions{Pattern: "shopping", Count: true})
	if err != nil {
		return fmt.Errorf("counting mode: %w", err)
	}
	if len(counts) != 2 {
		return fmt.Errorf("counting mode: %d rows, want 2", len(counts))
	}
	for _, h := range counts {
		if h.Count == 0 {
			return fmt.Errorf("counting mode: %q came back with a zero count", h.Path)
		}
	}

	// A pattern that matches nothing is exit 1, which must not read as failure.
	if _, err := fs.Grep(ctx, GrepOptions{Pattern: "zzz-no-such-text"}); err != nil {
		return fmt.Errorf("no-match search reported an error: %w", err)
	}
	return nil
}

// asExitError is errors.As with a concrete target, kept local so the import
// list stays short.
func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
