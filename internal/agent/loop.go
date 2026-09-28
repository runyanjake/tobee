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
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcphost"
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
	directive, err := l.states.RenderPhase("turn", StateData{})
	if err != nil {
		telemetry.Logger(ctx).Error("agent: turn directive failed", "err", err)
		return
	}
	conv.Append(llm.Message{Role: llm.RoleUser, Content: directive})
	t.React(reactPlanning)

	invalid, failures := 0, 0
	for step := 1; step <= l.maxSteps; step++ {
		sctx := telemetry.With(ctx, "step", step)
		d, err := decide(sctx, l.model, conv, l.offer())
		switch {
		case errors.Is(err, llm.ErrInvalidDecision):
			// The unreadable output is dropped so later calls can't continue it.
			if invalid++; invalid > 1 {
				return
			}
			conv.Append(llm.Message{Role: llm.RoleUser, Content: invalidNudge})
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
			l.use(sctx, t, call)
			if t.Await != nil {
				return
			}
		}
	}

	// Out of steps: one last call that can only reply.
	telemetry.Logger(ctx).Warn("agent: step budget spent; forcing a reply", "max_steps", l.maxSteps)
	conv.Append(llm.Message{Role: llm.RoleUser, Content: budgetNudge})
	d, err := decide(ctx, l.model, conv, []llm.ToolSpec{replySpec()})
	if err == nil {
		l.reply(ctx, t, d.Call)
		return
	}
	if len(t.Verbatim) > 0 {
		// A tool already rendered an answer; send that rather than nothing (D-030).
		t.Reply = strings.TrimSpace(renderReply(replyArgs{}, t.Verbatim))
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
	t.Reply = strings.TrimSpace(renderReply(args, t.Verbatim))
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

// use runs one MCP tool call and appends its result.
func (l *Loop) use(ctx context.Context, t *Turn, call llm.ToolCall) {
	name := call.Function.Name
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
}
