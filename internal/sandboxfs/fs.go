// Package sandboxfs is a typed filesystem rooted at a single directory.
// Every path handed to an FS method is validated and resolved inside Root;
// attempts to escape via .., absolute paths, or volume prefixes are rejected.
//
// It backs two distinct callers today: long-term agent memory under
// data/memory (see internal/tools/memory) and configurable host-file areas
// (see internal/workspace). The implementation is agnostic to what the root
// contains.
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

// FS is a sandboxed filesystem rooted at a single directory. All paths
// passed to its methods are interpreted relative to Root. MaxFileSize caps
// both Write and Append on this instance.
type FS struct {
	Root        string
	MaxFileSize int64
}

// NewFS resolves root to an absolute path, ensures it exists, and pairs it
// with the per-instance maxFileSize. A non-positive maxFileSize disables
// the cap (use only when the caller has its own bounds).
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

// resolve converts a caller-supplied relative path into an absolute
// filesystem path under Root, rejecting anything that would escape.
func (m *FS) resolve(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths not allowed: %q", rel)
	}
	// Forbid volume references on Windows (e.g. C:\foo, C:foo).
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

// Read returns the contents of a file relative to Root.
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

// Write replaces the contents of a file relative to Root. Rejects writes
// exceeding MaxFileSize. Intermediate directories are created.
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

// Append adds content to the end of a file, creating it if necessary.
// The combined file size must not exceed MaxFileSize.
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

// Exists reports whether the given relative path refers to a readable file.
func (m *FS) Exists(rel string) bool {
	abs, err := m.resolve(rel)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	return err == nil && !info.IsDir()
}

// List returns the relative paths of all regular files under the given
// relative directory, sorted lexicographically. Pass "" to list the root.
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

// SearchHit is one ripgrep-style match.
type SearchHit struct {
	Path    string
	Line    int
	Snippet string
}

// Search walks the root, returning substring matches (case-insensitive)
// across all regular files. Up to `limit` hits are returned.
func (m *FS) Search(query string, limit int) ([]SearchHit, error) {
	return m.SearchUnder(query, limit, "")
}

// SearchUnder walks a subdirectory of the root. relDir="" matches Search.
// Used to scope a search to a subtree.
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
