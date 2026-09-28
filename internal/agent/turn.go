package agent

import (
	"context"
	"strings"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/taskqueue"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// Turn carries the per-task state threaded through a strategy, from
// dequeue to deliver. Created once by the runtime, mutated in place by
// the strategy. Nothing on it outlives the turn except what Await hands
// to the queue (D-036).
type Turn struct {
	Ctx          context.Context
	Task         *taskqueue.Task
	Event        event.Event
	Conversation *Conversation

	// PlanMessageID is the connector's message ID for the user-facing
	// plan announcement. Set when the announcement is sent; used to edit
	// the message in place as step statuses change.
	PlanMessageID string

	// Reply is the text the runtime delivers to the event's origin.
	Reply string

	// Await is set when a tool asked the user a question. The runtime
	// parks the task instead of delivering a reply (D-036).
	Await *mcpserver.Await

	// Verbatim collects output from tools marked verbatim, in call order.
	// The synthesizer appends these blocks to the reply itself rather than
	// letting the model restate them (D-030).
	Verbatim []VerbatimBlock

	// Reactions are the emoji reactions added to the inbound message so
	// far, in order. On success the runtime clears them; on failure it
	// adds a failure marker and leaves the trail.
	Reactions []string

	out *delivery.Router
}

// VerbatimBlock is one tool's pre-rendered, user-facing output.
type VerbatimBlock struct {
	Tool string // tool that produced it, for logging
	Body string
}

// AddVerbatim records pre-rendered tool output, skipping blanks and
// exact duplicates — a model that calls status_summary twice in a turn
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

// Request is the conversation opening for this turn: the user's own
// words as their own message (D-029). A resumed task replays the original
// request, the question tobee asked, and the answer, as a normal chat.
func (t *Turn) Request() []llm.Message {
	if r := t.Task.Resume; r != nil {
		return []llm.Message{
			{Role: llm.RoleUser, Content: r.Request},
			{Role: llm.RoleAssistant, Content: r.Question},
			{Role: llm.RoleUser, Content: t.Event.Content},
		}
	}
	return []llm.Message{{Role: llm.RoleUser, Content: t.Event.Content}}
}

// React adds an emoji reaction to the inbound message and records it so
// the runtime can clear it later. Best-effort: no message ID (timers), a
// connector without reactions, or a transport error all degrade to a
// debug log — reactions are feedback, never load-bearing.
func (t *Turn) React(emoji string) {
	if t.Event.MessageID == "" || t.out == nil {
		return
	}
	if err := t.out.React(t.Ctx, t.Event.Origin, t.Event.MessageID, emoji, true); err != nil {
		telemetry.Logger(t.Ctx).Debug("agent: react failed; continuing", "err", err, "emoji", emoji)
		return
	}
	t.Reactions = append(t.Reactions, emoji)
}

// clearReactions removes every reaction React added, in order.
func (t *Turn) clearReactions() {
	if t.Event.MessageID == "" || t.out == nil {
		return
	}
	for _, emoji := range t.Reactions {
		if err := t.out.React(t.Ctx, t.Event.Origin, t.Event.MessageID, emoji, false); err != nil {
			telemetry.Logger(t.Ctx).Debug("agent: reaction removal failed; continuing", "err", err, "emoji", emoji)
		}
	}
	t.Reactions = nil
}
