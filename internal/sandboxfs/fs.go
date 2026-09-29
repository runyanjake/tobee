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
