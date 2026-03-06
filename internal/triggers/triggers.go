package triggers

import (
	"context"
	"log"
	"sync"
	"time"
	"tobee/internal/state"
)

// Trigger defines something that fires an action either on a schedule or in
// response to a named event. Set Interval > 0 for time-based triggers, or
// set Event to a non-empty string for event-based triggers.
type Trigger struct {
	Name     string
	Interval time.Duration     // time-based: how often to fire
	Event    string            // event-based: event name to listen for
	Action   string            // action name to invoke on the State
	Args     map[string]string // args forwarded to the action
}

// Engine manages all registered triggers and an event bus.
type Engine struct {
	mu       sync.Mutex
	triggers []Trigger
	events   chan string
	state    *state.State
}

func NewEngine(s *state.State) *Engine {
	return &Engine{
		triggers: make([]Trigger, 0),
		events:   make(chan string, 64),
		state:    s,
	}
}

// Register adds a trigger to the engine. Must be called before Start.
func (e *Engine) Register(t Trigger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.triggers = append(e.triggers, t)
}

// FireEvent pushes a named event onto the bus, triggering any matching event-based triggers.
func (e *Engine) FireEvent(event string) {
	select {
	case e.events <- event:
	default:
		log.Printf("triggers: event bus full, dropping event %q", event)
	}
}

// Start launches goroutines for all registered triggers. It returns immediately;
// cancel ctx to shut them down.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	snapshot := make([]Trigger, len(e.triggers))
	copy(snapshot, e.triggers)
	e.mu.Unlock()

	for _, t := range snapshot {
		if t.Interval > 0 {
			go e.runTimeBased(ctx, t)
		}
	}
	go e.runEventBased(ctx)
}

func (e *Engine) runTimeBased(ctx context.Context, t Trigger) {
	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.dispatch(ctx, t)
		}
	}
}

func (e *Engine) runEventBased(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-e.events:
			e.mu.Lock()
			snapshot := make([]Trigger, len(e.triggers))
			copy(snapshot, e.triggers)
			e.mu.Unlock()
			for _, t := range snapshot {
				if t.Event == event {
					go e.dispatch(ctx, t)
				}
			}
		}
	}
}

func (e *Engine) dispatch(ctx context.Context, t Trigger) {
	fn, ok := e.state.GetAction(t.Action)
	if !ok {
		log.Printf("trigger %q: action %q not found", t.Name, t.Action)
		return
	}
	result, err := fn(ctx, e.state, t.Args)
	if err != nil {
		log.Printf("trigger %q: action error: %v", t.Name, err)
		return
	}
	log.Printf("trigger %q fired: %s", t.Name, result)
}
