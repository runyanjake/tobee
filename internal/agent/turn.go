package agent

import (
	"context"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
)

// Turn carries the per-envelope state threaded through every phase of
// a turn (plan → announce → execute → synth → deliver). Created once at
// the top of processTurn, mutated in place by phases. There is no
// session state — each envelope is a standalone turn — and the
// conversation the LLM sees lives on Turn.Conversation.
type Turn struct {
	Ctx          context.Context
	Env          integrations.Envelope
	Conversation *Conversation

	// PlanMessageID is the integration's message ID for the user-facing
	// plan announcement. Set when the announcement is sent; used by
	// the loop to edit the message in place as step statuses change.
	// Empty when the integration does not produce an ID or when no
	// announcement was sent.
	PlanMessageID string

	// Reply is set by the synthesizer at the end of the turn. The
	// deliver step reads it and sends it to the integration.
	Reply string

	// Verbatim collects output from tools marked tools.Spec.Verbatim,
	// in call order. The synthesizer appends these blocks to the reply
	// itself rather than letting the model restate them — the model
	// contributes only the lead-in prose (D-030).
	Verbatim []VerbatimBlock

	// Reactions are the emoji reactions the loop has added to the
	// inbound message (Env.MessageID) so far, in order. On a successful
	// turn deliver clears them; on failure it adds a failure marker and
	// leaves the trail.
	Reactions []string
}

// VerbatimBlock is one tool's pre-rendered, user-facing output.
type VerbatimBlock struct {
	Tool string // tool that produced it, for logging
	Body string
}

// AddVerbatim records pre-rendered tool output, skipping blanks and
// exact duplicates — a model that calls status.summary twice in a turn
// should not produce the block twice.
func (t *Turn) AddVerbatim(tool, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	for _, v := range t.Verbatim {
		if v.Body == body {
			return
		}
	}
	t.Verbatim = append(t.Verbatim, VerbatimBlock{Tool: tool, Body: body})
}

// Plan returns the plan from the conversation once the planner phase
// has committed it, or nil if it hasn't.
func (t *Turn) Plan() *Plan {
	if t == nil || t.Conversation == nil {
		return nil
	}
	return t.Conversation.Plan
}
