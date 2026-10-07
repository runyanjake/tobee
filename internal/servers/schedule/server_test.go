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

// Asked to move a reminder, the model answered with a second schedule_create
// and left both to fire — and the new one's prompt was the user's instruction
// ("Change the reminder to 1pm instead of 3:30pm"), so the task text was lost
// (D-063).
func TestUpdateMovesAReminderInPlace(t *testing.T) {
	h, _, ctx := setup(t)

	res, err := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"Text Darren about if he can meet for basketball","at":"2026-10-10T15:30:00-07:00"}`))
	if err != nil || res.IsError {
		t.Fatalf("create = %+v, %v", res, err)
	}
	id := strings.Fields(strings.TrimPrefix(res.Text, "scheduled "))[0]

	res, err = h.Call(ctx, "schedule_update", json.RawMessage(
		`{"id":"`+id+`","at":"2026-10-10T13:00:00-07:00"}`))
	if err != nil || res.IsError {
		t.Fatalf("update = %+v, %v", res, err)
	}
	if !strings.Contains(res.Text, id) || !strings.Contains(res.Text, "1:00pm") {
		t.Fatalf("update result = %q", res.Text)
	}

	// One reminder, same id, new time, and the task text survived the move.
	list, err := h.Call(ctx, "schedule_list", json.RawMessage(`{}`))
	if err != nil || list.IsError {
		t.Fatalf("list = %+v, %v", list, err)
	}
	rows := strings.Split(strings.TrimSpace(list.Text), "\n")
	if len(rows) != 1 {
		t.Fatalf("got %d reminders, want 1:\n%s", len(rows), list.Text)
	}
	for _, want := range []string{id, "1:00pm", "Text Darren"} {
		if !strings.Contains(list.Text, want) {
			t.Fatalf("list missing %q:\n%s", want, list.Text)
		}
	}
	if strings.Contains(list.Text, "3:30pm") {
		t.Fatalf("the old fire time is still scheduled:\n%s", list.Text)
	}
}

func TestUpdateKeepsFieldsItWasNotGiven(t *testing.T) {
	h, _, ctx := setup(t)
	res, _ := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"Water the plants","at":"2026-10-10T15:30:00-07:00","name":"plants"}`))
	id := strings.Fields(strings.TrimPrefix(res.Text, "scheduled "))[0]

	// Reword only: the fire time must survive.
	if res, err := h.Call(ctx, "schedule_update", json.RawMessage(
		`{"id":"`+id+`","prompt":"Water the plants and feed the cat"}`)); err != nil || res.IsError {
		t.Fatalf("update = %+v, %v", res, err)
	}
	list, _ := h.Call(ctx, "schedule_list", json.RawMessage(`{}`))
	for _, want := range []string{"feed the cat", "3:30pm", "plants"} {
		if !strings.Contains(list.Text, want) {
			t.Fatalf("list missing %q:\n%s", want, list.Text)
		}
	}
}

func TestUpdateRejectsBadInput(t *testing.T) {
	h, _, ctx := setup(t)
	res, _ := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"Water the plants","at":"2026-10-10T15:30:00-07:00"}`))
	id := strings.Fields(strings.TrimPrefix(res.Text, "scheduled "))[0]

	for name, args := range map[string]string{
		"unknown id":     `{"id":"j-nope","at":"2026-10-10T13:00:00-07:00"}`,
		"nothing to do":  `{"id":"` + id + `"}`,
		"at and cron":    `{"id":"` + id + `","at":"2026-10-10T13:00:00-07:00","cron":"0 9 * * *"}`,
		"unparseable at": `{"id":"` + id + `","at":"next tuesdayish"}`,
	} {
		res, err := h.Call(ctx, "schedule_update", json.RawMessage(args))
		if err == nil && !res.IsError {
			t.Fatalf("%s was accepted: %q", name, res.Text)
		}
	}
	// The original is untouched by every one of those refusals.
	list, _ := h.Call(ctx, "schedule_list", json.RawMessage(`{}`))
	if !strings.Contains(list.Text, "3:30pm") || !strings.Contains(list.Text, "Water the plants") {
		t.Fatalf("a refused update disturbed the reminder:\n%s", list.Text)
	}
}

// A second create for the same thing is refused in code, naming the id, so an
// edit expressed as a create can't silently fork (D-054, D-063).
func TestCreateRefusesADuplicateOfAPendingReminder(t *testing.T) {
	h, _, ctx := setup(t)
	res, _ := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"Text Darren about if he can meet for basketball","at":"2026-10-10T15:30:00-07:00"}`))
	id := strings.Fields(strings.TrimPrefix(res.Text, "scheduled "))[0]

	// Same words, different punctuation and case, different time.
	dup, err := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"text darren about if he can meet for basketball!","at":"2026-10-10T13:00:00-07:00"}`))
	if err == nil && !dup.IsError {
		t.Fatalf("the duplicate was accepted: %q", dup.Text)
	}
	msg := dup.Text
	if err != nil {
		msg = err.Error()
	}
	for _, want := range []string{id, "schedule_update"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal should name %q, got: %s", want, msg)
		}
	}

	// A genuinely different reminder still goes through: the refusal is per
	// argument set, not a block on the tool.
	if res, err := h.Call(ctx, "schedule_create", json.RawMessage(
		`{"prompt":"Book the badminton court for Thursday","at":"2026-10-10T13:00:00-07:00"}`)); err != nil || res.IsError {
		t.Fatalf("an unrelated reminder was refused: %+v, %v", res, err)
	}
}

func TestNormalizePromptAndDuplicateFloor(t *testing.T) {
	if got := normalizePrompt("  Text Darren -- about BASKETBALL!! "); got != "text darren about basketball" {
		t.Fatalf("normalizePrompt = %q", got)
	}
	// Short prompts are exempt from the containment test, so two unrelated
	// terse reminders don't collide.
	pending := []scheduler.Job{{ID: "j-1", Prompt: "call mom"}}
	if _, ok := duplicatePending(pending, "call dad"); ok {
		t.Fatal("short unrelated prompts were treated as duplicates")
	}
}
