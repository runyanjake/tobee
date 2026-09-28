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

// Turn is per-task state; nothing outlives it except what Await hands to the queue (D-036).
type Turn struct {
	Ctx          context.Context
	Task         *taskqueue.Task
	Event        event.Event
	Conversation *Conversation

	// PlanMessageID is the announcement to edit as step statuses change.
	PlanMessageID string

	Reply string

	// Await set means the runtime parks the task instead of replying (D-036).
	Await *mcpserver.Await

	// Verbatim is appended to the reply by code, not restated by the model (D-030).
	Verbatim []VerbatimBlock

	// Reactions are cleared on success; on failure the trail stays.
	Reactions []string

	out *delivery.Router
}

type VerbatimBlock struct {
	Tool string // for logging
	Body string
}

// AddVerbatim skips blanks and exact duplicates, e.g. status_summary called twice.
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

// Plan is nil until the planner commits.
func (t *Turn) Plan() *Plan {
	if t == nil || t.Conversation == nil {
		return nil
	}
	return t.Conversation.Plan
}

// Request is the user's words as their own message (D-029); a resume replays request, question, answer.
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

// React is best-effort: reactions are feedback, never load-bearing.
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
