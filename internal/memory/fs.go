// Package memory is a typed filesystem over data/memory — the agent's
// long-term file-based store. Every path handed to an FS method is
// validated and resolved inside Root; attempts to escape via .. or
// absolute paths are rejected.
package memory

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

// MaxFileSize caps writes and reads to keep memory human-manageable.
const MaxFileSize = 64 * 1024

// FS is a sandboxed filesystem rooted at a single directory. All paths
// passed to its methods are interpreted relative to Root.
type FS struct {
	Root string
}

// NewFS resolves root to an absolute path and ensures it exists.
func NewFS(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir root: %w", err)
	}
	return &FS{Root: abs}, nil
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
	if len(content) > MaxFileSize {
		return fmt.Errorf("content exceeds %d bytes", MaxFileSize)
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
	if existing+int64(len(content)) > MaxFileSize {
		return fmt.Errorf("append would exceed %d bytes", MaxFileSize)
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

// Search walks the memory root, returning substring matches (case-insensitive)
// across all regular files. Up to `limit` hits are returned.
func (m *FS) Search(query string, limit int) ([]SearchHit, error) {
	if query == "" {
		return nil, errors.New("empty query")
	}
	if limit <= 0 {
		limit = 20
	}
	needle := strings.ToLower(query)

	var hits []SearchHit
	err := filepath.WalkDir(m.Root, func(path string, d fs.DirEntry, err error) error {
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
