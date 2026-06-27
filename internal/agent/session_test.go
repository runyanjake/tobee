package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
)

func TestSessionStoreGet_LazyRotatesIdleSession(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root, 4, 100*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	key := "discord:chan-A"
	sess := store.Get(key, false)
	sess.Append(llm.Message{Role: llm.RoleUser, Content: "hello"})

	if err := store.WriteSummary(key, "rolling summary"); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}

	// Backdate so the next Get sees the session as idle.
	sess.lastActivity = time.Now().Add(-1 * time.Hour)
	stale := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(store.SummaryPath(key), stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(store.recentPath(key), stale, stale); err != nil {
		t.Fatalf("chtimes recent: %v", err)
	}

	fresh := store.Get(key, false)
	if fresh == sess {
		t.Fatalf("expected a brand-new Session instance after rotation")
	}
	if len(fresh.Recent()) != 0 {
		t.Fatalf("expected empty Recent() after rotation, got %d", len(fresh.Recent()))
	}
	if got := store.ReadSummary(key); got != "" {
		t.Fatalf("expected current.md gone after rotation, got %q", got)
	}

	archiveDir := filepath.Join(root, "discord", "chan-A", "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("ReadDir archive: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one archive file, got %d", len(entries))
	}
}

func TestSessionStoreGet_RotatesStaleFileWithoutInMemory(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root, 4, 100*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	key := "discord:chan-B"
	if err := store.WriteSummary(key, "leftover"); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	stale := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(store.SummaryPath(key), stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	fresh := store.Get(key, false)
	if len(fresh.Recent()) != 0 {
		t.Fatalf("expected empty Recent() for fresh session")
	}
	if got := store.ReadSummary(key); got != "" {
		t.Fatalf("expected current.md rotated away, got %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(root, "discord", "chan-B", "archive"))
	if err != nil {
		t.Fatalf("ReadDir archive: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one archive file, got %d", len(entries))
	}
}

func TestSessionStoreSweepIdle_RotatesFromDisk(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root, 4, 50*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	key := "discord:chan-C"
	if err := store.WriteSummary(key, "stale on disk"); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	stale := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(store.SummaryPath(key), stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := store.SweepIdle(); got != 1 {
		t.Fatalf("SweepIdle: expected 1 rotated, got %d", got)
	}
	if got := store.ReadSummary(key); got != "" {
		t.Fatalf("expected current.md rotated, got %q", got)
	}
}

func TestSessionStoreGet_DoesNotRotateActiveSession(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root, 4, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	key := "discord:chan-D"
	first := store.Get(key, false)
	first.Append(llm.Message{Role: llm.RoleUser, Content: "hi"})

	second := store.Get(key, false)
	if second != first {
		t.Fatalf("expected same Session instance for non-idle channel")
	}
	if len(second.Recent()) != 1 {
		t.Fatalf("expected Recent() preserved, got %d", len(second.Recent()))
	}
}

func TestSessionStoreGet_RestoresRecentFromDisk(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root, 4, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	key := "discord:chan-restore"
	sess := store.Get(key, false)
	sess.Append(llm.Message{Role: llm.RoleUser, Content: "first"})
	sess.Append(llm.Message{Role: llm.RoleAssistant, Content: "reply"})

	// Simulate a process restart by building a new store over the same dir.
	store2, err := NewSessionStore(root, 4, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewSessionStore restart: %v", err)
	}
	resumed := store2.Get(key, false)
	got := resumed.Recent()
	if len(got) != 2 || got[0].Content != "first" || got[1].Content != "reply" {
		t.Fatalf("expected ring restored from recent.json, got %+v", got)
	}
}

func TestSessionStoreGet_DMUsesDMIdleTimeout(t *testing.T) {
	root := t.TempDir()
	// Channel idle = 50ms (very short), DM idle = 1h. A DM that's been
	// idle 1s must survive; a channel idle 1s must rotate.
	store, err := NewSessionStore(root, 4, 50*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	dmKey := "discord:dm-x"
	dm := store.Get(dmKey, true)
	dm.Append(llm.Message{Role: llm.RoleUser, Content: "alive"})
	dm.lastActivity = time.Now().Add(-1 * time.Second)

	chKey := "discord:chan-x"
	ch := store.Get(chKey, false)
	ch.Append(llm.Message{Role: llm.RoleUser, Content: "doomed"})
	ch.lastActivity = time.Now().Add(-1 * time.Second)

	if dmAgain := store.Get(dmKey, true); dmAgain != dm {
		t.Fatalf("DM should not have rotated under DM idle timeout")
	}
	if chAgain := store.Get(chKey, false); chAgain == ch {
		t.Fatalf("channel should have rotated under channel idle timeout")
	}
}

func TestSessionStore_IdleDisabledWhenTimeoutZero(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root, 4, 0, 0)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	key := "discord:chan-E"
	if err := store.WriteSummary(key, "ancient"); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	stale := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(store.SummaryPath(key), stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := store.SweepIdle(); got != 0 {
		t.Fatalf("expected no rotations when idleTimeout=0, got %d", got)
	}
	if got := store.ReadSummary(key); got != "ancient" {
		t.Fatalf("expected current.md untouched, got %q", got)
	}
}
