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
	budgetNudge  = "You cannot make more tool calls on this turn. Reply now with what you have, and say what wasn't done."
	verbatimNote = "That output is already shown to the user with your reply. Don't repeat it, " +
		"and don't ask for it again — answer what was actually asked."
	// A repeat is refused by argument set, never by tool: widening a search
	// after it came back empty is the right next move, so the note asks for it.
	repeatNote = "Same arguments as earlier in this turn, so that was not run again. " +
		"If this answers the question, reply now. If you need different information, " +
		"call it with different arguments or call another tool."
	closedNote = "Same arguments again. %s is closed for the rest of this turn: " +
		"call a different tool or reply with what you have."
)

const (
	// maxSuppressed is how many refused repeats a tool gets before it is
	// closed. Refusing the exact call is not enough on its own: the context
	// barely changes, so a low-temperature model makes the same choice
	// again (D-051).
	maxSuppressed = 2

	// maxStaleCalls ends a turn that has stopped learning anything. This
	// replaces a cap on the number of calls: counting calls punished long
	// honest work — a coarse search, three reads, two writes is fine — while
	// still letting a tight loop spend the whole budget. What matters is
	// whether a call returned something new, not how many there have been
	// (D-057).
	maxStaleCalls = 3

	// replyReserve is held back from the turn budget so there is always time
	// for the closing reply rather than ending on a bare ❌. It is capped at a
	// quarter of the budget, so a short budget still gets to do some work.
	replyReserve = 15 * time.Second

	// maxConversationBytes is a backstop, not a budget: without a call cap, a
	// pathological turn could otherwise grow until the model's context
	// window overflows.
	maxConversationBytes = 192 * 1024
)

// Loop is the tool-calling agent loop (ReAct): each model call picks one
// tool, until the model calls reply. Planning and replying are tools the
// model chooses, not fixed phases, so a greeting costs one call (D-043).
type Loop struct {
	model  llm.Model
	tools  *mcphost.Host
	states *StateTemplates
	out    *delivery.Router
}

func NewLoop(model llm.Model, host *mcphost.Host, states *StateTemplates, out *delivery.Router) *Loop {
	return &Loop{model: model, tools: host, states: states, out: out}
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
	directive, err := l.states.RenderPhase("turn", StateData{Kind: string(t.Event.Kind)})
	if err != nil {
		telemetry.Logger(ctx).Error("agent: turn directive failed", "err", err)
		return
	}
	conv.AppendHarness(llm.Message{Role: llm.RoleUser, Content: directive})
	t.React(reactPlanning)

	invalid, failures := 0, 0
	seen := map[string]string{} // call key → result, cleared when anything changes
	st := newRepeatState()
	reserve := replyReserve
	if dl, ok := ctx.Deadline(); ok {
		if quarter := time.Until(dl) / 4; quarter < reserve {
			reserve = quarter
		}
	}
	for step := 1; ; step++ {
		if stop := l.stalled(ctx, conv, st, reserve); stop.reason != "" {
			// One line per cause, so each is greppable on its own tag (D-059).
			telemetry.Log(ctx, slog.LevelWarn, telemetry.Action, "agent: "+stop.event,
				"steps", t.Steps, "stale", st.stale, "closed", len(st.closed),
				"distinct_results", len(st.results), "conversation_bytes", conv.Bytes(),
				"reason", stop.reason)
			t.AddProblem("stalled", "", stop.reason)
			t.PromoteProblems()
			break
		}
		t.Steps = step
		sctx := telemetry.With(ctx, "step", step)
		d, err := decide(sctx, l.model, conv, l.offer(st.closed))
		switch {
		case errors.Is(err, llm.ErrInvalidDecision):
			// The unreadable output is dropped so later calls can't continue it.
			if invalid++; invalid > 1 {
				t.AddProblem("unreadable", "", "I couldn't put together a usable tool call, twice over, so I stopped")
				return
			}
			t.AddRecovered("unreadable", "", "I couldn't put together a usable tool call and tried the step again")
			conv.AppendHarness(llm.Message{Role: llm.RoleUser, Content: invalidNudge})
			continue
		case err != nil:
			if failures++; failures > 1 || ctx.Err() != nil {
				t.AddProblem("model_error", "", "I couldn't finish thinking this through: "+firstLine(err.Error()))
				return
			}
			t.AddRecovered("model_error", "", "I hit an error thinking and tried again: "+firstLine(err.Error()))
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
			l.use(sctx, t, call, seen, st)
			if t.Await != nil {
				return
			}
		}
	}

	// Stalled or out of time: one last call that can only reply.
	conv.AppendHarness(llm.Message{Role: llm.RoleUser, Content: budgetNudge})
	d, err := decide(ctx, l.model, conv, []llm.ToolSpec{replySpec()})
	if err == nil {
		l.reply(ctx, t, d.Call)
		return
	}
	t.AddProblem("model_error", "", "I couldn't put my reply together")
	// A tool may already have rendered an answer, and the problem list is worth
	// sending even on its own: silence hides what went wrong (D-051).
	t.Reply = strings.TrimSpace(renderReply(replyArgs{}, t.Verbatim, t.Actions, t.Problems))
}

// offer is the host's whole catalog plus the loop's own tools (D-029), minus
// any the turn has closed. Closing is enforced by leaving the tool out of the
// decision schema, so the model cannot call it again (D-051).
func (l *Loop) offer(closed map[string]bool) []llm.ToolSpec {
	catalog := l.tools.Tools()
	if len(closed) > 0 {
		open := make([]llm.ToolSpec, 0, len(catalog))
		for _, t := range catalog {
			if !closed[t.Name] {
				open = append(open, t)
			}
		}
		catalog = open
	}
	return append(catalog, replySpec(), llm.ToolSpec{
		Name: planTool,
		Description: "Only for work with several distinct steps: set the checklist the user sees. " +
			"Call again with updated statuses as steps finish. Never for a single lookup or a chat reply.",
		InputSchema: planSchema,
		Category:    llm.CategoryFinish,
	})
}

// repeatState tracks refused repeats per tool, which tools that has closed,
// and whether the turn is still learning anything (D-051, D-057).
type repeatState struct {
	suppressed map[string]int
	closed     map[string]bool
	results    map[string]bool // every distinct result this turn
	stale      int             // consecutive calls that returned nothing new
}

func newRepeatState() *repeatState {
	return &repeatState{suppressed: map[string]int{}, closed: map[string]bool{}, results: map[string]bool{}}
}

// sawResult records a tool result and reports whether it was new. A call whose
// answer is already in the conversation has not moved the turn forward, even
// when the call itself was not an exact repeat.
func (s *repeatState) sawResult(content string) bool {
	if s.results[content] {
		s.stale++
		return false
	}
	s.results[content] = true
	s.stale = 0
	return true
}

// stop says why a turn should make no more calls: event names the log line,
// reason is the sentence the user will read (D-053).
type stop struct {
	event  string
	reason string
}

// stalled decides whether the turn should stop making calls. Each cause gets
// its own log event so a loop in production can be found by tag rather than by
// reading a field (D-057, D-059).
func (l *Loop) stalled(ctx context.Context, conv *Conversation, st *repeatState, reserve time.Duration) stop {
	switch {
	case st.stale >= maxStaleCalls:
		return stop{"turn stalled", fmt.Sprintf(
			"I made %d calls in a row that told me nothing new, so I stopped digging", st.stale)}
	case conv.Bytes() > maxConversationBytes:
		return stop{"turn too large", "this turn got too large to keep working on, so it may be unfinished"}
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < reserve {
		return stop{"turn out of time", "I ran out of time before finishing, so this may be unfinished"}
	}
	return stop{}
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
	t.Reply = strings.TrimSpace(renderReply(args, t.Verbatim, t.Actions, t.Problems))
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

// use runs one MCP tool call and appends its result. A call with arguments
// already used this turn is not run again; the same tool with different
// arguments always is. Only a tool that keeps being called identically is
// closed, and then by name, as the last way to break a stuck loop (D-051).
func (l *Loop) use(ctx context.Context, t *Turn, call llm.ToolCall, seen map[string]string, st *repeatState) {
	name := call.Function.Name
	key := name + "\x00" + canonicalJSON(call.Function.Arguments)
	if prev, ok := seen[key]; ok {
		note := repeatNote
		st.suppressed[name]++
		detail := fmt.Sprintf("I called %s twice the same way, so the second one didn't run", name)
		if st.suppressed[name] >= maxSuppressed {
			st.closed[name] = true
			note = fmt.Sprintf(closedNote, name)
			detail = fmt.Sprintf("I kept calling %s the same way (%d times), so I stopped using it",
				name, st.suppressed[name]+1)
		}
		st.stale++ // a cached answer is by definition nothing new
		telemetry.Log(ctx, slog.LevelWarn, telemetry.Action, "agent: tool call repeated; not run",
			"tool", name, "call_id", call.ID, "suppressed", st.suppressed[name], "stale", st.stale,
			telemetry.Content("args", call.Function.Arguments))
		if st.closed[name] {
			telemetry.Log(ctx, slog.LevelWarn, telemetry.Action, "agent: tool closed",
				"tool", name, "suppressed", st.suppressed[name], "for", "the rest of this turn")
		}
		// Recorded for the session either way, but only shown if the turn ends
		// badly: a repeat the loop absorbed and then answered correctly is
		// self-correction, not something to warn about (D-055).
		t.AddRecovered("repeated", name, detail)
		t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: name, Content: prev})
		t.Conversation.AppendHarness(llm.Message{Role: llm.RoleUser, Content: note})
		return
	}

	telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: tool call",
		"tool", name, "call_id", call.ID, telemetry.Content("args", call.Function.Arguments))
	start := time.Now()
	res, err := l.tools.Call(ctx, name, json.RawMessage(call.Function.Arguments))
	content, note := res.Text, ""
	var rendered []string // verbatim tools closed because the answer is now rendered
	// sig is what the call actually told us, before any note code adds to it:
	// two tools returning the same data must look the same, or a loop that
	// alternates between them reads as progress (D-057).
	sig := res.Text
	status, level := "ok", slog.LevelInfo
	switch {
	case err != nil:
		content = fmt.Sprintf("error: %v", err)
		sig = content
		status, level = "failed", slog.LevelWarn
		t.AddProblem("tool_error", name, fmt.Sprintf("I tried %s and it failed: %s", name, firstLine(err.Error())))
	case res.IsError:
		content = "error: " + res.Text
		sig = content
		status, level = "error", slog.LevelWarn
		t.AddProblem("tool_error", name, fmt.Sprintf("I tried %s and it failed: %s", name, firstLine(res.Text)))
	case res.Await != nil:
		status = "await"
		t.Await = res.Await
	case res.Verbatim:
		t.AddVerbatim(name, res.Text)
		status = "verbatim"
		note = verbatimNote
		// The answer is rendered. Calling another verbatim tool only stacks a
		// second report on top of it, which is how a shopping-list request came
		// back as a status dump (D-060).
		rendered = l.tools.VerbatimTools()
		for _, v := range rendered {
			st.closed[v] = true
		}
	}
	fresh := st.sawResult(sig)
	telemetry.Log(ctx, level, telemetry.Action, "agent: tool result",
		"tool", name, "call_id", call.ID, "status", status, "fresh", fresh,
		"duration_ms", time.Since(start).Milliseconds(), telemetry.Content("content", res.Text))
	if len(rendered) > 0 {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: tool closed",
			"tool", name, "reason", "it rendered the answer", "tools", rendered)
	}
	// The result is what happened and is saved to the session; a note is harness
	// text for this turn only, so it rides in its own message and never ends up
	// in a transcript (D-029, D-060).
	t.Conversation.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: name, Content: content})
	if note != "" {
		t.Conversation.AppendHarness(llm.Message{Role: llm.RoleUser, Content: note})
	}

	cat := l.tools.Category(name)
	if cat == llm.CategoryRead {
		seen[key] = content
		return
	}
	// Anything that isn't a pure read may have changed what a read returns,
	// so earlier results are stale and repeating one is no longer a loop.
	clear(seen)
	clear(st.suppressed)
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
		t.AddProblem("declined", p.Tool, fmt.Sprintf("I didn't run %s because you didn't approve it", p.Tool))
		return
	}
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: approval granted", "tool", p.Tool)
	l.use(ctx, t, call, map[string]string{}, newRepeatState())
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
