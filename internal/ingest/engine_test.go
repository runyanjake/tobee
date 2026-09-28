package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/event"
)

type sink struct {
	mu  sync.Mutex
	got []event.Event
}

func (s *sink) Enqueue(ev event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev)
	return nil
}

func (s *sink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// fixed emits a fixed list of events, then blocks until cancelled.
type fixed struct {
	name   string
	events []event.Event
}

func (f fixed) Name() string { return f.name }

func (f fixed) Run(ctx context.Context, emit Emit) error {
	for _, ev := range f.events {
		emit(ev)
	}
	<-ctx.Done()
	return nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEngineDedupsAndAllowlists(t *testing.T) {
	s := &sink{}
	e := New(s)
	e.Allow("mail", []string{"Me@Example.com"})
	_ = e.Register(fixed{name: "mail", events: []event.Event{
		{ID: "1", Actor: event.Actor{ID: "me@example.com"}, Content: "hi"},
		{ID: "1", Actor: event.Actor{ID: "me@example.com"}, Content: "hi again"}, // duplicate
		{ID: "2", Actor: event.Actor{ID: "spam@example.com"}, Content: "buy"},    // not allowed
		{ID: "3", Content: "no actor"},                                           // timers pass
	}})

	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	waitFor(t, func() bool { return s.len() == 2 })
	cancel()
	e.Wait()

	if s.got[0].Source != "mail" || s.got[0].Content != "hi" || s.got[1].ID != "3" {
		t.Fatalf("admitted = %+v", s.got)
	}
}

// flaky fails its first run, then behaves.
type flaky struct {
	mu   sync.Mutex
	runs int
}

func (f *flaky) Name() string { return "flaky" }

func (f *flaky) Run(ctx context.Context, emit Emit) error {
	f.mu.Lock()
	f.runs++
	first := f.runs == 1
	f.mu.Unlock()
	if first {
		return errors.New("connection refused")
	}
	emit(event.Event{ID: "ok"})
	<-ctx.Done()
	return nil
}

func TestEngineRestartsFailedSource(t *testing.T) {
	s := &sink{}
	e := New(s)
	_ = e.Register(&flaky{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	waitFor(t, func() bool { return s.len() == 1 })
}

func TestEngineRegisterWhileRunning(t *testing.T) {
	s := &sink{}
	e := New(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)

	if err := e.Register(fixed{name: "late", events: []event.Event{{ID: "x"}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.len() == 1 })
	if err := e.Register(fixed{name: "late"}); err == nil {
		t.Fatal("duplicate source name accepted")
	}
	if err := e.Unregister("late"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Names()); n != 0 {
		t.Fatalf("%d sources after unregister", n)
	}
}
