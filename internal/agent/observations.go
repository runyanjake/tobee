package agent

import (
	"context"
	"log"
	"sync"
	"time"
)

// Observation defines something that fires an action either on a schedule or in
// response to a named event. Set Interval > 0 for time-based observations, or
// set Event to a non-empty string for event-based observations.
type Observation struct {
	Name     string
	Interval time.Duration     // time-based: how often to fire
	Event    string            // event-based: event name to listen for
	Action   string            // action name to invoke on the State
	Args     map[string]string // args forwarded to the action
}

// ObservationEngine manages all registered observations and an event bus.
type ObservationEngine struct {
	mu           sync.Mutex
	observations []Observation
	events       chan string
	state        *State
}

func NewObservationEngine(s *State) *ObservationEngine {
	return &ObservationEngine{
		observations: make([]Observation, 0),
		events:       make(chan string, 64),
		state:        s,
	}
}

// Register adds an observation to the engine. Must be called before Start.
func (e *ObservationEngine) Register(o Observation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observations = append(e.observations, o)
}

// FireEvent pushes a named event onto the bus, triggering any matching event-based observations.
func (e *ObservationEngine) FireEvent(event string) {
	select {
	case e.events <- event:
	default:
		log.Printf("observations: event bus full, dropping event %q", event)
	}
}

// Start launches goroutines for all registered observations. It returns immediately;
// cancel ctx to shut them down.
func (e *ObservationEngine) Start(ctx context.Context) {
	e.mu.Lock()
	snapshot := make([]Observation, len(e.observations))
	copy(snapshot, e.observations)
	e.mu.Unlock()

	for _, o := range snapshot {
		if o.Interval > 0 {
			go e.runTimeBased(ctx, o)
		}
	}
	go e.runEventBased(ctx)
}

func (e *ObservationEngine) runTimeBased(ctx context.Context, o Observation) {
	ticker := time.NewTicker(o.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.dispatch(ctx, o)
		}
	}
}

func (e *ObservationEngine) runEventBased(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-e.events:
			e.mu.Lock()
			snapshot := make([]Observation, len(e.observations))
			copy(snapshot, e.observations)
			e.mu.Unlock()
			for _, o := range snapshot {
				if o.Event == event {
					go e.dispatch(ctx, o)
				}
			}
		}
	}
}

func (e *ObservationEngine) dispatch(ctx context.Context, o Observation) {
	fn, ok := e.state.GetAction(o.Action)
	if !ok {
		log.Printf("observation %q: action %q not found", o.Name, o.Action)
		return
	}
	result, err := fn(ctx, e.state, o.Args)
	if err != nil {
		log.Printf("observation %q: action error: %v", o.Name, err)
		return
	}
	log.Printf("observation %q fired: %s", o.Name, result)
}
