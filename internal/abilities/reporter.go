// Package abilities holds cross-cutting plumbing for things tobee *does*
// outside the LLM turn — surfacing what subsystems are up to, scheduled
// work, recent background activity, and (later) higher-level capabilities
// composed from them.
//
// The first primitive here is the Reporter contract used by the
// status.report tool. Any subsystem (scheduler, janitor, integration, …)
// can register a Reporter; the status tool snapshots them on demand and
// returns one composed JSON blob to the model.
//
// Each Reporter is responsible for its own relevance filtering — what
// counts as "stale" or "noisy" varies by subsystem. The ability layer
// stays a dumb composer.
package abilities

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// ReportData is one Reporter's view of its state, split into three
// time-axis buckets the model can speak about naturally.
//
// All three are optional; an empty bucket should be left as nil so the
// composed JSON stays compact.
type ReportData struct {
	Doing   json.RawMessage `json:"doing,omitempty"`   // active right now
	Done    json.RawMessage `json:"done,omitempty"`    // since `since`, recent and relevant
	Waiting json.RawMessage `json:"waiting,omitempty"` // scheduled or pending
}

// Reporter is implemented by anything that wants to surface state through
// the status ability. Name is used as the JSON key in the composed output
// and must be unique within a Registry.
type Reporter interface {
	Name() string
	Report(ctx context.Context, since time.Time) (ReportData, error)
}

// Registry holds the set of Reporters status.report aggregates over.
type Registry struct {
	mu   sync.RWMutex
	reps map[string]Reporter
}

func NewRegistry() *Registry {
	return &Registry{reps: make(map[string]Reporter)}
}

// Register adds a Reporter. Later registrations under the same name
// overwrite earlier ones — convenient for testing, harmless in main wiring
// where names are programmer-chosen.
func (r *Registry) Register(rep Reporter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reps[rep.Name()] = rep
}

// Names returns registered reporter names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.reps))
	for n := range r.reps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Snapshot calls every Reporter and returns a map keyed by Name. Reporter
// errors are recorded under that reporter's slot rather than aborting the
// whole snapshot — partial state beats no state.
func (r *Registry) Snapshot(ctx context.Context, since time.Time) map[string]ReportData {
	r.mu.RLock()
	reps := make([]Reporter, 0, len(r.reps))
	for _, rep := range r.reps {
		reps = append(reps, rep)
	}
	r.mu.RUnlock()

	out := make(map[string]ReportData, len(reps))
	for _, rep := range reps {
		data, err := rep.Report(ctx, since)
		if err != nil {
			msg, _ := json.Marshal(map[string]string{"error": err.Error()})
			out[rep.Name()] = ReportData{Doing: msg}
			continue
		}
		out[rep.Name()] = data
	}
	return out
}
