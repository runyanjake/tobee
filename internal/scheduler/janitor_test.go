package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJanitorPruneArchives_RemovesOldFiles(t *testing.T) {
	root := t.TempDir()
	archiveDir := filepath.Join(root, "discord", "chan-A", "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldFile := filepath.Join(archiveDir, "old.md")
	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, stale, stale); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	freshFile := filepath.Join(archiveDir, "fresh.md")
	if err := os.WriteFile(freshFile, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("write fresh: %v", err)
	}

	j := NewJanitor(nil, root, 0, 24*time.Hour, time.Hour)
	if got := j.pruneArchives(); got != 1 {
		t.Fatalf("pruneArchives: expected 1, got %d", got)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("expected old file gone, got err=%v", err)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Fatalf("expected fresh file kept, got err=%v", err)
	}
}

func TestJanitorPruneArchives_IgnoresNonArchiveDirs(t *testing.T) {
	root := t.TempDir()
	currentPath := filepath.Join(root, "discord", "chan-B", "current.md")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte("active summary"), 0o644); err != nil {
		t.Fatalf("write current: %v", err)
	}
	stale := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(currentPath, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	j := NewJanitor(nil, root, 0, time.Hour, time.Hour)
	if got := j.pruneArchives(); got != 0 {
		t.Fatalf("pruneArchives must skip current.md, got %d removed", got)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("expected current.md untouched, got %v", err)
	}
}

func TestJanitorPruneEmptyDirs_RemovesEmptyButKeepsRoot(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "discord", "chan-C", "archive")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	j := NewJanitor(nil, root, 0, 0, time.Hour)
	j.pruneEmptyDirs()

	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("expected empty archive dir removed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "discord", "chan-C")); !os.IsNotExist(err) {
		t.Fatalf("expected channel dir removed when empty, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "discord")); !os.IsNotExist(err) {
		t.Fatalf("expected integration dir removed when empty, got %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("expected root preserved, got %v", err)
	}
}

func TestJanitorPruneEmptyDirs_KeepsNonEmpty(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "discord", "chan-D")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	current := filepath.Join(keep, "current.md")
	if err := os.WriteFile(current, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	j := NewJanitor(nil, root, 0, 0, time.Hour)
	j.pruneEmptyDirs()

	if _, err := os.Stat(current); err != nil {
		t.Fatalf("expected file preserved, got %v", err)
	}
}
