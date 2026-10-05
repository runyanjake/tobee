package schedule

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/scheduler"
	"github.com/runyanjake/tobee/internal/scope"
)

func setup(t *testing.T) (*mcphost.Host, *scheduler.JobManager, context.Context) {
	t.Helper()
	store, err := scheduler.NewJobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobs := scheduler.NewJobManager(store)
	h := mcphost.New()
	t.Cleanup(h.Close)
	if err := h.ConnectInProcess(context.Background(), New("", jobs)); err != nil {
		t.Fatal(err)
	}
	ctx := scope.With(context.Background(), scope.UserScope{
		Connector: "discord", User: "u1", UserName: "jake", Channel: "c1",
	})
	return h, jobs, ctx
}

func pinnedBlock(t *testing.T, h *mcphost.Host, ctx context.Context) string {
	t.Helper()
	pinned, err := h.Pinned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pinned {
		if strings.Contains(p.Text, "<reminders>") {
			return p.Text
		}
	}
	return ""
}

// What is waiting is context, not something to spend a call discovering. It is
// derived from the job store, so there is no second copy to keep in step (D-059).
func TestPendingRemindersArePinned(t *testing.T) {
	h, _, ctx := setup(t)

	if got := pinnedBlock(t, h, ctx); got != "" {
		t.Fatalf("nothing is scheduled, yet a block was pinned: %q", got)
	}

	res, err := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"see if the AliExpress order has shipped","at":"in 2h","name":"aliexpress"}`))
	if err != nil || res.IsError {
		t.Fatalf("create = %+v, %v", res, err)
	}

	block := pinnedBlock(t, h, ctx)
	for _, want := range []string{"aliexpress", "see if the AliExpress order has shipped"} {
		if !strings.Contains(block, want) {
			t.Fatalf("pinned block missing %q:\n%s", want, block)
		}
	}

	// Firing or cancelling removes it with no bookkeeping: the block is derived.
	id := strings.Fields(strings.TrimPrefix(res.Text, "scheduled "))[0]
	if _, err := h.Call(ctx, "schedule_cancel", json.RawMessage(`{"id":"`+id+`"}`)); err != nil {
		t.Fatal(err)
	}
	if got := pinnedBlock(t, h, ctx); got != "" {
		t.Fatalf("a cancelled reminder is still pinned:\n%s", got)
	}
}

// One person's reminders must not reach another's prompt, and a turn with no
// user pins nothing.
func TestPendingRemindersAreScopedToTheUser(t *testing.T) {
	h, _, ctx := setup(t)
	if _, err := h.Call(ctx, "schedule_create", json.RawMessage(`{"prompt":"mine","at":"in 1h"}`)); err != nil {
		t.Fatal(err)
	}

	other := scope.With(context.Background(), scope.UserScope{
		Connector: "discord", User: "u2", Channel: "c1",
	})
	if got := pinnedBlock(t, h, other); got != "" {
		t.Fatalf("another user saw the reminder:\n%s", got)
	}
	if got := pinnedBlock(t, h, context.Background()); got != "" {
		t.Fatalf("a turn with no user pinned reminders:\n%s", got)
	}
}

// The confirmation reports the id and the fire time; the block carries the note.
func TestCreateConfirmationNamesTheFireTime(t *testing.T) {
	h, _, ctx := setup(t)
	res, err := h.Call(ctx, "schedule_create", json.RawMessage(`{"prompt":"leave now","at":"in 30m"}`))
	if err != nil || res.IsError {
		t.Fatalf("create = %+v, %v", res, err)
	}
	if !strings.Contains(res.Text, "fires") || !strings.Contains(res.Text, "scheduled j-") {
		t.Fatalf("confirmation = %q", res.Text)
	}
}
