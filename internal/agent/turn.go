package agent

import (
	"context"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
)

// Turn carries the per-envelope state threaded through every phase of
// a turn (think-act loop, then synthesis). Created once at the top of
// processTurn, mutated in place by the loop body and synthesizer.
type Turn struct {
	Ctx        context.Context
	Env        integrations.Envelope
	Session    *Session
	Transcript []llm.Message

	// Reply is set by the synthesizer at the end of the turn. The
	// deliver step reads it and sends it to the integration.
	Reply string
}
