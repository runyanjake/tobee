package agent

import (
	"context"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
)

// Turn carries the per-envelope state threaded through every phase of
// the agent state machine. Created once at the top of processTurn,
// mutated in place by phases. Adding a new state grows this struct
// instead of every phase signature.
type Turn struct {
	Ctx        context.Context
	Env        integrations.Envelope
	Session    *Session
	Transcript []llm.Message

	// Set by phaseTriage; nil until then.
	Triage *TriageResult

	// Set when triage commits a plan (Category == TriageCategoryPlan).
	// Mutated by phaseExec and phaseReplan.
	Plan *Plan

	// Set by whichever phase produced the user-facing text. Read by
	// the deliver step after the state machine terminates.
	Reply string
}

// phaseFn is one state in the agent state machine. It mutates Turn in
// place and returns the next phase to run, or nil to end the turn.
// A non-nil error aborts the turn (logged + reply skipped). Phase-level
// recovery belongs inside the phase, the same way replan is internal
// to the exec arc.
type phaseFn func(t *Turn) (next phaseFn, err error)
