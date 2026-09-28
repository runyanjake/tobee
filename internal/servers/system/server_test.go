package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/runyanjake/tobee/internal/mcphost"
)

// Filename order is the system prompt's order (D-012), and edits apply
// on the next read without a restart.
func TestPinsPromptFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "01-voice.md"), []byte("voice\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "00-identity.md"), []byte("identity\n"), 0o644)

	srv, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := mcphost.New()
	defer h.Close()
	if err := h.ConnectInProcess(context.Background(), srv); err != nil {
		t.Fatal(err)
	}

	got, _ := h.Pinned(context.Background())
	if len(got) != 2 || got[0].Text != "identity" || got[1].Text != "voice" {
		t.Fatalf("Pinned() = %+v", got)
	}

	_ = os.WriteFile(filepath.Join(dir, "01-voice.md"), []byte("new voice"), 0o644)
	if got, _ := h.Pinned(context.Background()); got[1].Text != "new voice" {
		t.Fatalf("edit not picked up: %+v", got)
	}
}

func TestNoPromptFilesIsAnError(t *testing.T) {
	if _, err := New(t.TempDir()); err == nil {
		t.Fatal("empty prompts dir accepted")
	}
}
