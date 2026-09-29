package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/taskqueue"
	"github.com/runyanjake/tobee/internal/telemetry"
)

const (
	replyTool = "reply"
	planTool  = "plan"
)

// Protocol scaffolding, like tool descriptions, lives in code.
const (
	invalidNudge = "Your last response could not be read as a tool call. Call exactly one tool."
	budgetNudge  = "You are out of steps. Reply now with what you have, and say what wasn't done."
	verbatimNote = "\n\n(Shown to the user as-is with your reply. Don't repeat it.)"
	repeatNote   = "\n\n(Same call as earlier in this turn; nothing has changed since. Use this result.)"
)

// Loop is the tool-calling agent loop (ReAct): each model call picks one
// tool, until the model calls reply. Planning and replying are tools the
// model chooses, not fixed phases, so a greeting costs one call (D-043).
type Loop struct {
	model    llm.Model
	tools    *mcphost.Host
	states   *StateTemplates
	out      *delivery.Router
	maxSteps int
}

func NewLoop(model llm.Model, host *mcphost.Host, states *StateTemplates, out *delivery.Router, maxSteps int) *Loop {
	if maxSteps <= 0 {
		maxSteps = 12
	}
	return &Loop{model: model, tools: host, states: states, out: out, maxSteps: maxSteps}
}

func (l *Loop) Name() string { return "react" }

func (l *Loop) Handle(t *Turn) {
	ctx, conv := t.Ctx, t.Conversation

	// The user's words are their own messages; the directive is separate and tagged (D-029).
	for _, m := range t.Request() {
		conv.Append(m)
	}
	if r := t.Task.Resume; r != nil && r.Pending != nil {
		l.resolveApproval(ctx, t, r.Pending)
	}
	directive, err := l.states.RenderPhase("turn", StateData{})
	if err != nil {
		telemetry.Logger(ctx).Error("agent: turn directive failed", "err", err)
		return
	}
	conv.AppendHarness(llm.Message{Role: llm.RoleUser, Content: directive})
	t.React(reactPlanning)

	invalid, failures := 0, 0
	seen := map[string]string{} // call key → result, cleared when anything changes
	for step := 1; step <= l.maxSteps; step++ {
		sctx := telemetry.With(ctx, "step", step)
		d, err := decide(sctx, l.model, conv, l.offer())
		switch {
		case errors.Is(err, llm.ErrInvalidDecision):
			// The unreadable output is dropped so later calls can't continue it.
			if invalid++; invalid > 1 {
				return
			}
			conv.AppendHarness(llm.Message{Role: llm.RoleUser, Content: invalidNudge})
			continue
		case err != nil:
			if failures++; failures > 1 || ctx.Err() != nil {
				return
			}
			continue
		}

		call := d.Call
		conv.Append(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}})
		switch call.Function.Name {
		case replyTool:
			l.reply(sctx, t, call)
			return
		case planTool:
			l.plan(sctx, t, call)
		default:
			if len(t.Reactions) < 3 {
				t.React(reactExecuting)
			}
			if l.tools.NeedsApproval(call.Function.Name) {
				l.askApproval(sctx, t, call)
				return
			}
			l.use(sctx, t, call, seen)
			if t.Await != nil {
				return
			}
		}
	}

	// Out of steps: one last call that can only reply.
	telemetry.Logger(ctx).Warn("agent: step budget spent; forcing a reply", "max_steps", l.maxSteps)
	conv.AppendHarness(llm.Message{Role: llm.RoleUser, Content: budgetNudge})
	d, err := decide(ctx, l.model, conv, []llm.ToolSpec{replySpec()})
	if err == nil {
		l.reply(ctx, t, d.Call)
		return
	}
	if len(t.Verbatim) > 0 {
		// A tool already rendered an answer; send that rather than nothing (D-030).
		t.Reply = strings.TrimSpace(renderReply(replyArgs{}, t.Verbatim, t.Actions))
	}
}

// offer is the host's whole catalog plus the loop's own tools (D-029).
func (l *Loop) offer() []llm.ToolSpec {
	return append(l.tools.Tools(), replySpec(), llm.ToolSpec{
		Name: planTool,
		Description: "Only for work with several distinct steps: set the checklist the user sees. " +
			"Call again with updated statuses as steps finish. Never for a single lookup or a chat reply.",
		InputSchema: planSchema,
		Category:    llm.CategoryFinish,
	})
}

func replySpec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: replyTool,
		Description: "Answer the user and end the turn. Use it as soon as you can answer; " +
			"greetings and small talk need no other tool.",
		InputSchema: replySchema,
		Category:    llm.CategoryFinish,
	}
}

func (l *Loop) reply(ctx context.Context, t *Turn, call llm.ToolCall) {
	var args replyArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		telemetry.Logger(ctx).Error("agent: reply decode failed", "err", err)
		return
	}
	t.Reply = strings.TrimSpace(renderReply(args, t.Verbatim, t.Actions))
}

func (l *Loop) plan(ctx context.Context, t *Turn, call llm.ToolCall) {
	var p Plan
	ack := "ok"
	if err := json.Unmarshal([]byte(call.Function.Arguments), &p); err != nil {
		ack = fmt.Sprintf("error: %v", err)
	} else {
		t.Plan = &p
		titles := make([]string, len(p.Items))
		for i, it := range p.Items {
			titles[i] = it.Status + ": " + it.Title
		}
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: plan", "goal", p.Goal, "steps", titles)
		l.showPlan(ctx, t)
	}
	t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: call.Function.Name, Content: ack})
}

// showPlan sends or edits the checklist, only where the connector can edit
// it later and only when there is more than one step.
func (l *Loop) showPlan(ctx context.Context, t *Turn) {
	origin := t.Event.Origin
	if len(t.Plan.Items) < minShownItems || !l.out.CanEdit(origin.Connector) {
		return
	}
	msg := t.Plan.Render()
	if t.PlanMessageID != "" {
		if err := l.out.Edit(ctx, origin, t.PlanMessageID, msg); err != nil {
			telemetry.Logger(ctx).Debug("agent: plan edit failed; continuing", "err", err)
		}
		return
	}
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Output, "agent: output",
		"kind", "plan", "connector", origin.Connector, "channel", origin.Channel, telemetry.Content("content", msg))
	id, err := l.out.Send(ctx, origin, msg)
	if err != nil {
		telemetry.Logger(ctx).Warn("agent: plan send failed; continuing without it", "err", err)
		return
	}
	t.PlanMessageID = id
}

// use runs one MCP tool call and appends its result. A repeat of an
// earlier identical call returns the earlier result without running it,
// unless something has changed since.
func (l *Loop) use(ctx context.Context, t *Turn, call llm.ToolCall, seen map[string]string) {
	name := call.Function.Name
	key := name + "\x00" + canonicalJSON(call.Function.Arguments)
	if prev, ok := seen[key]; ok {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: tool call repeated; not run",
			"tool", name, "call_id", call.ID)
		t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: name, Content: prev + repeatNote})
		return
	}

	telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: tool call",
		"tool", name, "call_id", call.ID, telemetry.Content("args", call.Function.Arguments))
	start := time.Now()
	res, err := l.tools.Call(ctx, name, json.RawMessage(call.Function.Arguments))
	content := res.Text
	status, level := "ok", slog.LevelInfo
	switch {
	case err != nil:
		content = fmt.Sprintf("error: %v", err)
		status, level = "failed", slog.LevelWarn
	case res.IsError:
		content = "error: " + res.Text
		status, level = "error", slog.LevelWarn
	case res.Await != nil:
		status = "await"
		t.Await = res.Await
	case res.Verbatim:
		t.AddVerbatim(name, res.Text)
		status = "verbatim"
		content += verbatimNote
	}
	telemetry.Log(ctx, level, telemetry.Action, "agent: tool result",
		"tool", name, "call_id", call.ID, "status", status,
		"duration_ms", time.Since(start).Milliseconds(), telemetry.Content("content", res.Text))
	t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: name, Content: content})

	cat := l.tools.Category(name)
	if cat == llm.CategoryRead {
		seen[key] = content
		return
	}
	// Anything that isn't a pure read may have changed what a read returns.
	clear(seen)
	if status != "await" {
		t.Actions = append(t.Actions, Action{Tool: name, OK: status == "ok" || status == "verbatim", Result: firstLine(content)})
	}
}

// askApproval parks the turn behind a code-written question; the call runs
// only if the user says yes (D-047). The model never phrases the question,
// so it can't soften or misstate what will happen.
func (l *Loop) askApproval(ctx context.Context, t *Turn, call llm.ToolCall) {
	question := fmt.Sprintf("Confirm: %s %s\nReply yes to go ahead; anything else cancels.",
		call.Function.Name, compactJSON(call.Function.Arguments))
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: approval requested",
		"tool", call.Function.Name, "call_id", call.ID, telemetry.Content("args", call.Function.Arguments))

	origin := t.Event.Origin
	keys := []string{event.ActorKey(origin, t.Event.Actor.ID)}
	if p := t.Event.Actor.Person; p != "" {
		keys = append(keys, event.PersonKey(p))
	}
	if msgID, err := l.out.Send(ctx, origin, question); err != nil {
		telemetry.Logger(ctx).Error("agent: approval question not sent; call dropped", "err", err)
		t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: call.Function.Name,
			Content: "error: could not ask the user for approval; nothing was run"})
		return
	} else if msgID != "" {
		keys = append([]string{event.ReplyKey(origin, msgID)}, keys...)
	}
	t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: call.Function.Name,
		Content: "Waiting for the user to approve. Nothing has run yet."})
	t.Await = &mcpserver.Await{Question: question, Keys: keys}
	t.Pending = &taskqueue.PendingCall{ID: call.ID, Tool: call.Function.Name, Arguments: call.Function.Arguments}
}

// resolveApproval runs or cancels the call a resumed task was waiting on,
// exactly as proposed; the model does not get to re-decide it.
func (l *Loop) resolveApproval(ctx context.Context, t *Turn, p *taskqueue.PendingCall) {
	call := llm.ToolCall{ID: p.ID, Type: "function", Function: llm.FunctionCall{Name: p.Tool, Arguments: p.Arguments}}
	t.Conversation.Append(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}})
	if !approved(t.Event.Content) {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: approval declined", "tool", p.Tool)
		t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: p.ID, Name: p.Tool,
			Content: "Not run: the user did not approve."})
		t.Actions = append(t.Actions, Action{Tool: p.Tool, OK: false, Result: "not run: not approved"})
		return
	}
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: approval granted", "tool", p.Tool)
	l.use(ctx, t, call, map[string]string{})
}

// approved accepts only a plain yes. Anything else — including silence
// turned into a new request — cancels, the safe default for destruction.
func approved(answer string) bool {
	a := strings.ToLower(strings.TrimSpace(answer))
	a = strings.TrimRight(a, ".!")
	switch a {
	case "yes", "y", "yeah", "yep", "yup", "ok", "okay", "sure", "confirm", "confirmed",
		"approve", "approved", "do it", "go ahead", "yes please", "please do":
		return true
	}
	return false
}

func canonicalJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, _ := json.Marshal(v) // map keys are sorted
	return string(b)
}

func compactJSON(raw string) string {
	c := canonicalJSON(raw)
	if len(c) > 500 {
		c = c[:500] + "…"
	}
	return c
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
