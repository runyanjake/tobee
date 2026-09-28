package taskqueue

import (
	"context"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/event"
)

func msg(id, actor, content, inReplyTo string) event.Event {
	return event.Event{
		ID: id, Source: "discord", Kind: event.KindMessage,
		Actor:     event.Actor{ID: actor},
		Origin:    event.Address{Connector: "discord", Channel: "c1"},
		InReplyTo: inReplyTo,
		Content:   content,
	}
}

func next(t *testing.T, q *Queue) *Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	task, err := q.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return task
}

// Tasks survive a restart until Done.
func TestQueuePersistsUntilDone(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(msg("1", "u", "first", ""))
	_ = q.Enqueue(msg("2", "u", "second", ""))
	first := next(t, q)
	q.Done(first)

	q2, err := Open(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, q2); got.Event.Content != "second" {
		t.Fatalf("after reopen got %q, want second", got.Event.Content)
	}
}

// A task that keeps crashing the process is dropped, not replayed forever.
func TestQueueDropsPoisonTask(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir, 10)
	_ = q.Enqueue(msg("1", "u", "poison", ""))
	for i := 0; i < maxAttempts; i++ {
		next(t, q) // dequeued, never Done: simulates a crash mid-turn
		q, _ = Open(dir, 10)
	}
	if pending, _ := q.Stats(); pending != 0 {
		t.Fatalf("poison task still pending after %d attempts", maxAttempts)
	}
}

func TestParkAndResume(t *testing.T) {
	q, _ := Open(t.TempDir(), 10)
	_ = q.Enqueue(msg("1", "u", "rename the file", ""))
	task := next(t, q)

	addr := task.Event.Origin
	keys := []string{event.ReplyKey(addr, "q-msg"), event.ActorKey(addr, "u")}
	if err := q.Park(task, "Which file?", keys); err != nil {
		t.Fatalf("Park: %v", err)
	}

	// Someone else talking in the channel does not resume it.
	_ = q.Enqueue(msg("2", "other", "hello", ""))
	if got := next(t, q); got.Resume != nil {
		t.Fatal("another user's message resumed the task")
	}

	// The answer, as a reply to the question, does.
	_ = q.Enqueue(msg("3", "u", "notes.md", "q-msg"))
	got := next(t, q)
	if got.Resume == nil {
		t.Fatal("answer did not resume the parked task")
	}
	if got.Resume.Request != "rename the file" || got.Resume.Question != "Which file?" {
		t.Fatalf("Resume = %+v", got.Resume)
	}
	if _, parked := q.Stats(); parked != 0 {
		t.Fatal("parked task not consumed")
	}
}

func TestParkedTaskExpires(t *testing.T) {
	q, _ := Open(t.TempDir(), 10)
	_ = q.Enqueue(msg("1", "u", "do it", ""))
	task := next(t, q)
	_ = q.Park(task, "Sure?", []string{event.ActorKey(task.Event.Origin, "u")})
	for _, p := range q.parked {
		p.Asked = time.Now().Add(-ParkTTL - time.Minute)
	}

	_ = q.Enqueue(msg("2", "u", "yes", ""))
	if got := next(t, q); got.Resume != nil {
		t.Fatal("expired question was resumed")
	}
}

func TestQueueCapacity(t *testing.T) {
	q, _ := Open(t.TempDir(), 1)
	if err := q.Enqueue(msg("1", "u", "a", "")); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(msg("2", "u", "b", "")); err != ErrFull {
		t.Fatalf("err = %v, want ErrFull", err)
	}
}
