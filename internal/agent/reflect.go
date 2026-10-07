package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/session"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// lessonsTool is the reflector's only offered tool, so the answer is a
// schema-constrained choice like every other model call (D-041).
const lessonsTool = "lessons"

var lessonsSchema = json.RawMessage(`{
  "type": "object",
  "required": ["lessons"],
  "properties": {
    "lessons": {
      "type": "array",
      "maxItems": 3,
      "description": "One short imperative line each, or none at all if nothing general can be drawn.",
      "items": {"type": "string"}
    }
  }
}`)

// Reflector turns a closed session's failures into lessons for the next one
// (D-052). It is the only model call outside a turn, it runs on a session
// that actually failed, and it sees only what code recorded — never the
// model's own earlier chatter.
type Reflector struct {
	model  llm.Model
	states *StateTemplates
	save   func(person string, lessons []string) error
}

func NewReflector(model llm.Model, states *StateTemplates, save func(string, []string) error) *Reflector {
	return &Reflector{model: model, states: states, save: save}
}

// Reflect is best-effort: a failure here must not fail the archive, or the
// session would be retried and its transcript written twice.
func (r *Reflector) Reflect(ctx context.Context, sess *session.Session) {
	if r == nil || sess == nil {
		return
	}
	facts := failureFacts(sess)
	if facts == "" {
		return // nothing failed; there is nothing to learn
	}
	directive, err := r.states.RenderPhase("reflect", StateData{})
	if err != nil {
		slog.Error("reflect: directive failed", "err", err)
		return
	}

	conv := &Conversation{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: facts},
		{Role: llm.RoleUser, Content: directive},
	}}
	d, err := decide(ctx, r.model, conv, []llm.ToolSpec{{
		Name: lessonsTool,
		Description: "Record what to do differently next time: which tool answers a kind of question, " +
			"what to read before acting, an order of steps that worked. Never about the person or " +
			"anything they said, and never about repeating or varying calls — the harness handles that.",
		InputSchema: lessonsSchema,
		Category:    llm.CategoryFinish,
	}})
	if err != nil {
		slog.Warn("reflect: no lessons drawn", "person", sess.Person, "err", err)
		return
	}
	var out struct {
		Lessons []string `json:"lessons"`
	}
	if err := json.Unmarshal([]byte(d.Call.Function.Arguments), &out); err != nil {
		slog.Warn("reflect: lessons unreadable", "person", sess.Person, "err", err)
		return
	}
	if len(out.Lessons) == 0 {
		return
	}
	if err := r.save(sess.Person, out.Lessons); err != nil {
		slog.Error("reflect: lessons not saved", "person", sess.Person, "err", err)
		return
	}
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "reflect: lessons saved",
		"person", sess.Person, "count", len(out.Lessons), telemetry.Content("lessons", strings.Join(out.Lessons, " | ")))
}

// failureFacts is the request and the recorded outcome of every turn that went
// wrong, and nothing else: the reflection is grounded in what code saw, not in
// what the model said at the time (D-051).
func failureFacts(sess *session.Session) string {
	var b strings.Builder
	for _, ex := range sess.Exchanges {
		if ex.Outcome == nil || len(ex.Outcome.Problems) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\nRequest: %s\n", oneLine(firstUserMessage(ex.Messages)))
		for _, a := range ex.Outcome.Acted {
			fmt.Fprintf(&b, "  did: %s\n", oneLine(a))
		}
		for _, p := range ex.Outcome.Problems {
			fmt.Fprintf(&b, "  went wrong: %s\n", oneLine(p))
		}
		fmt.Fprintf(&b, "  ended: %s after %d step(s)\n", ex.Outcome.Status, ex.Outcome.Steps)
	}
	if b.Len() == 0 {
		return ""
	}
	return "<failures>" + b.String() + "</failures>"
}

func firstUserMessage(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	return "(unknown request)"
}
