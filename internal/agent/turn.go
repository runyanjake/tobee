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

	// Plan is set once the model calls the plan tool; PlanMessageID is its
	// progress message, edited as the model updates it.
	Plan          *Plan
	PlanMessageID string

	Reply string

	// Await set means the runtime parks the task instead of replying (D-036);
	// Pending is the tool call waiting on the user's approval, if that's why (D-047).
	Await   *mcpserver.Await
	Pending *taskqueue.PendingCall

	// Actions records every tool that changed something or reached outside,
	// with its real outcome. Code renders them under the reply (D-047).
	Actions []Action

	// Problems records what went wrong or was refused, including on reads,
	// which produce no Action. Code renders them under the reply and the
	// runtime saves them to the session, so a failed turn can't be reported
	// as a clean one (D-051).
	Problems []Problem

	// Steps counts the model calls this turn spent.
	Steps int

	// Verbatim is appended to the reply by code, not restated by the model (D-030).
	Verbatim []VerbatimBlock

	// Reactions are cleared on success; on failure the trail stays.
	Reactions []string

	out *delivery.Router
}

// Action is one state-changing or outward tool call and what really happened.
type Action struct {
	Tool   string
	OK     bool
	Result string // first line of the tool's result or error
}

type VerbatimBlock struct {
	Tool string // for logging
	Body string
}

// Problem is one thing that did not work this turn. Kind is a short code so
// the session can be read by machine as well as by a person. A Recovered
// problem is kept for the session but not shown to the user: a retry that
// worked is history, not an outcome.
type Problem struct {
	Kind      string // tool_error | repeated | budget | unreadable | model_error | declined
	Tool      string // empty when the problem isn't about one tool
	Detail    string
	Recovered bool
}

// AddProblem records a problem the user should hear about, replacing a
// recovered entry for the same tool: one entry per kind and tool, so a tool
// that fails twice for one reason is one line, not two.
func (t *Turn) AddProblem(kind, tool, detail string) { t.problem(kind, tool, detail, false) }

// AddRecovered records something that went wrong and was then handled.
func (t *Turn) AddRecovered(kind, tool, detail string) { t.problem(kind, tool, detail, true) }

func (t *Turn) problem(kind, tool, detail string, recovered bool) {
	for i, p := range t.Problems {
		if p.Kind == kind && p.Tool == tool {
			t.Problems[i].Detail = detail // the latest detail is the fullest
			t.Problems[i].Recovered = p.Recovered && recovered
			return
		}
	}
	t.Problems = append(t.Problems, Problem{Kind: kind, Tool: tool, Detail: detail, Recovered: recovered})
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
