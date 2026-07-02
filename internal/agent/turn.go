package agent

import (
	"context"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
)

// Turn carries the per-envelope state threaded through every phase of
// a turn (plan → announce → execute → synth). Created once at the top
// of processTurn, mutated in place by phases.
type Turn struct {
	Ctx        context.Context
	Env        integrations.Envelope
	Session    *Session
	Transcript []llm.Message

	// Plan is set by the planner phase; non-nil for the rest of the
	// turn (the planner's text-wrap fallback guarantees a plan even
	// when the model bypasses the tool protocol).
	Plan *Plan

	// PlanMessageID is the integration's message ID for the user-facing
	// plan announcement. Set when the announcement is sent; used by
	// the loop to edit the message in place as step statuses change.
	// Empty when the integration does not produce an ID or when no
	// announcement was sent.
	PlanMessageID string

	// Reply is set by the synthesizer at the end of the turn. The
	// deliver step reads it and sends it to the integration.
	Reply string

	// Reactions are the emoji reactions the loop has added to the
	// inbound message (Env.MessageID) so far, in order. On a successful
	// turn deliver clears them; on failure it adds a failure marker and
	// leaves the trail.
	Reactions []string
}
