// Package sandboxfs is a filesystem confined to one root directory; it backs
// memory and workspace areas. Paths escaping Root are rejected (D-003).
package sandboxfs

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FS interprets every path relative to Root; MaxFileSize caps Write and Append.
type FS struct {
	Root        string
	MaxFileSize int64
}

// NewFS creates root if needed. A non-positive maxFileSize disables the cap.
func NewFS(root string, maxFileSize int64) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir root: %w", err)
	}
	return &FS{Root: abs, MaxFileSize: maxFileSize}, nil
}

// resolve is the security boundary: it rejects absolute, volume-qualified,
// and ..-escaping paths so model input can never leave Root (D-003).
func (m *FS) resolve(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths not allowed: %q", rel)
	}
	// Windows volume references (C:\foo, C:foo) are not absolute but still escape.
	if vol := filepath.VolumeName(rel); vol != "" {
		return "", fmt.Errorf("volume-qualified paths not allowed: %q", rel)
	}
	cleaned := filepath.Clean(rel)
	if strings.HasPrefix(cleaned, "..") || cleaned == ".." {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}
	abs := filepath.Join(m.Root, cleaned)
	relCheck, err := filepath.Rel(m.Root, abs)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}
	return abs, nil
}

func (m *FS) Read(rel string) (string, error) {
	abs, err := m.resolve(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Write creates intermediate directories.
func (m *FS) Write(rel, content string) error {
	if m.MaxFileSize > 0 && int64(len(content)) > m.MaxFileSize {
		return fmt.Errorf("content exceeds %d bytes", m.MaxFileSize)
	}
	abs, err := m.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// Append checks MaxFileSize against the combined size.
func (m *FS) Append(rel, content string) error {
	abs, err := m.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	info, statErr := os.Stat(abs)
	existing := int64(0)
	if statErr == nil {
		existing = info.Size()
	}
	if m.MaxFileSize > 0 && existing+int64(len(content)) > m.MaxFileSize {
		return fmt.Errorf("append would exceed %d bytes", m.MaxFileSize)
	}
	f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

func (m *FS) Exists(rel string) bool {
	abs, err := m.resolve(rel)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	return err == nil && !info.IsDir()
}

// List returns sorted regular-file paths under relDir; "" lists the root.
func (m *FS) List(relDir string) ([]string, error) {
	start := m.Root
	if relDir != "" {
		abs, err := m.resolve(relDir)
		if err != nil {
			return nil, err
		}
		start = abs
	}
	var files []string
	err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(m.Root, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

type SearchHit struct {
	Path    string
	Line    int
	Snippet string
}

// Search returns up to limit case-insensitive substring matches.
func (m *FS) Search(query string, limit int) ([]SearchHit, error) {
	return m.SearchUnder(query, limit, "")
}

// SearchUnder is Search scoped to relDir.
func (m *FS) SearchUnder(query string, limit int, relDir string) ([]SearchHit, error) {
	if query == "" {
		return nil, errors.New("empty query")
	}
	if limit <= 0 {
		limit = 20
	}
	needle := strings.ToLower(query)

	start := m.Root
	if relDir != "" {
		abs, err := m.resolve(relDir)
		if err != nil {
			return nil, err
		}
		start = abs
	}

	var hits []SearchHit
	err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(m.Root, path)
		rel = filepath.ToSlash(rel)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNum := 0
		for sc.Scan() {
			lineNum++
			line := sc.Text()
			if strings.Contains(strings.ToLower(line), needle) {
				hits = append(hits, SearchHit{
					Path:    rel,
					Line:    lineNum,
					Snippet: strings.TrimSpace(line),
				})
				if len(hits) >= limit {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, err
	}
	return hits, nil
}

// Delete removes one file. Directories are refused: a model-chosen path
// should never take a whole subtree with it.
func (m *FS) Delete(rel string) error {
	abs, err := m.resolve(rel)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory", rel)
	}
	return os.Remove(abs)
}

// Rename moves a file or directory; the target must not exist.
func (m *FS) Rename(from, to string) error {
	src, err := m.resolve(from)
	if err != nil {
		return err
	}
	dst, err := m.resolve(to)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%q already exists", to)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// DirExists reports whether rel is an existing directory.
func (m *FS) DirExists(rel string) bool {
	abs, err := m.resolve(rel)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	return err == nil && info.IsDir()
}

// LineRange is an inclusive 1-based span of lines.
type LineRange struct {
	From int
	To   int
}

func (r LineRange) String() string {
	if r.From == r.To {
		return fmt.Sprintf("L%d", r.From)
	}
	return fmt.Sprintf("L%d-%d", r.From, r.To)
}

// FileHits is one file's coarse result: how many lines matched and the blocks
// they sit in, so the next call can read exactly those spans (D-056).
type FileHits struct {
	Path   string
	Count  int
	Blocks []LineRange
}

// Blocks are runs of non-blank lines. That is the only structure shared by
// markdown, plain text, code and config, so coarse search never assumes a
// format. A run longer than maxBlockLines is clamped around the hit, which
// keeps one minified line or a CSV from returning the whole file.
const (
	maxBlockLines   = 40
	maxSearchFileSz = 1 << 20 // read fully below this; stream line-only above
)

// SearchFiles is the coarse rung: one row per matching file with its blocks.
func (m *FS) SearchFiles(query, relDir string, limit int) ([]FileHits, error) {
	if query == "" {
		return nil, errors.New("empty query")
	}
	if limit <= 0 {
		limit = 20
	}
	needle := strings.ToLower(query)
	var out []FileHits
	err := m.walkFiles(relDir, func(rel string, lines []string) bool {
		var blocks []LineRange
		count := 0
		for i, line := range lines {
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			count++
			blocks = append(blocks, blockAround(lines, i+1))
		}
		if count == 0 {
			return true
		}
		out = append(out, FileHits{Path: rel, Count: count, Blocks: mergeRanges(blocks)})
		return len(out) < limit
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count // the most relevant file first
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Slice returns the given line ranges of a file, in order, each with the range
// it came from. Out-of-range spans are clamped; an empty list returns nothing.
func (m *FS) Slice(rel string, ranges []LineRange) ([]string, []LineRange, error) {
	body, err := m.Read(rel)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(body, "\n")
	var parts []string
	var got []LineRange
	for _, r := range ranges {
		from, to := r.From, r.To
		if from < 1 {
			from = 1
		}
		if to > len(lines) {
			to = len(lines)
		}
		if from > len(lines) || to < from {
			continue
		}
		parts = append(parts, strings.Join(lines[from-1:to], "\n"))
		got = append(got, LineRange{From: from, To: to})
	}
	return parts, got, nil
}

// Lines returns a file's line count, for callers that need to bound a range.
func (m *FS) Lines(rel string) (int, error) {
	body, err := m.Read(rel)
	if err != nil {
		return 0, err
	}
	return len(strings.Split(body, "\n")), nil
}

// ContextAround returns the lines surrounding a hit, for the fine rung.
func (m *FS) ContextAround(rel string, line, around int) ([]string, LineRange, error) {
	body, err := m.Read(rel)
	if err != nil {
		return nil, LineRange{}, err
	}
	lines := strings.Split(body, "\n")
	from, to := line-around, line+around
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > len(lines) {
		return nil, LineRange{}, nil
	}
	return lines[from-1 : to], LineRange{From: from, To: to}, nil
}

// walkFiles calls fn with each file's lines; returning false stops the walk.
// Files above maxSearchFileSz are skipped rather than read into memory.
func (m *FS) walkFiles(relDir string, fn func(rel string, lines []string) bool) error {
	start := m.Root
	if relDir != "" {
		abs, err := m.resolve(relDir)
		if err != nil {
			return err
		}
		start = abs
	}
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable entries
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > maxSearchFileSz {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(m.Root, p)
		if !fn(filepath.ToSlash(rel), strings.Split(string(b), "\n")) {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return err
	}
	return nil
}

// blockAround grows from a 1-based line to the blank lines either side of it.
func blockAround(lines []string, line int) LineRange {
	i := line - 1
	from, to := i, i
	for from > 0 && strings.TrimSpace(lines[from-1]) != "" {
		from--
	}
	for to < len(lines)-1 && strings.TrimSpace(lines[to+1]) != "" {
		to++
	}
	if to-from+1 > maxBlockLines {
		// Centre the window on the hit, then shift it inside the file.
		from = i - (maxBlockLines-1)/2
		if from < 0 {
			from = 0
		}
		to = from + maxBlockLines - 1
		if to > len(lines)-1 {
			to = len(lines) - 1
			if from = to - maxBlockLines + 1; from < 0 {
				from = 0
			}
		}
	}
	return LineRange{From: from + 1, To: to + 1}
}

// mergeRanges collapses overlapping and adjacent spans.
func mergeRanges(in []LineRange) []LineRange {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i].From < in[j].From })
	out := []LineRange{in[0]}
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r.From <= last.To+1 {
			if r.To > last.To {
				last.To = r.To
			}
			continue
		}
		out = append(out, r)
	}
	return out
}
