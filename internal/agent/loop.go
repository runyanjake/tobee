package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/scope"
)

// Reaction emojis mark turn progress on the inbound message: received,
// planning, executing, and (terminal) failure. A successful turn clears
// its reactions instead of leaving a marker.
const (
	reactReceived  = "✅"
	reactPlanning  = "🧠"
	reactExecuting = "💭"
	reactFailed    = "❌"
)

// Config controls the agent's execution limits at the turn level.
// Per-step and total executor caps live on Executor.
type Config struct {
	TurnBudget time.Duration // wall-clock cap on a single turn
}

// Agent is the serial worker that consumes envelopes from the bus and
// drives each through plan → announce → execute → synth → deliver.
// One goroutine processes envelopes sequentially. Each envelope is a
// standalone turn — no session state carries between turns.
type Agent struct {
	bus     *integrations.Bus
	ctxb    *ContextBuilder
	replies *Replies

	planner *Planner
	exec    *Executor
	synth   *Synthesizer

	cfg Config
}

func New(
	bus *integrations.Bus,
	ctxb *ContextBuilder,
	replies *Replies,
	planner *Planner,
	exec *Executor,
	synth *Synthesizer,
	cfg Config,
) *Agent {
	if cfg.TurnBudget <= 0 {
		cfg.TurnBudget = 2 * time.Minute
	}
	return &Agent{
		bus: bus, ctxb: ctxb,
		replies: replies,
		planner: planner, exec: exec, synth: synth,
		cfg: cfg,
	}
}

// Start launches the single worker goroutine. Cancel ctx to stop.
func (a *Agent) Start(ctx context.Context) {
	go func() {
		slog.Info("agent: loop started", "senders", a.replies.Summary())
		for {
			select {
			case <-ctx.Done():
				slog.Info("agent: loop stopped")
				return
			case env := <-a.bus.C():
				slog.Debug("agent: envelope dequeued",
					"integration", env.Integration, "channel", env.Channel,
					"user", env.User, "content_chars", len(env.Content))
				a.processTurn(ctx, env)
			}
		}
	}()
}

// processTurn drives one envelope through:
//
//  1. Plan (Planner.Run): commits a typed Plan via the plan.commit
//     tool with tool_choice=required. Protocol violations are retried
//     once, then abort the turn.
//  2. Announce: the plan is rendered as a Discord message with per-step
//     emoji statuses and sent. The returned message ID is stored on
//     the Turn so subsequent step transitions edit this same message.
//     Skipped when the plan has no tool-bearing steps.
//  3. Execute: for each step in order, RunStep does the per-step ReAct
//     sub-loop. The model must call a real tool or the virtual
//     step.finish tool; free-form text is a protocol violation.
//     No-tools steps (respond-only, e.g. greetings) are no-ops.
//  4. Synth: composes the user-facing reply via the reply.commit tool.
//     Its input is the structured plan + step results + original user
//     message — not the act loop's assistant messages.
//  5. Deliver: send the reply to the integration.
func (a *Agent) processTurn(parent context.Context, env integrations.Envelope) {
	ctx, cancel := context.WithTimeout(parent, a.cfg.TurnBudget)
	defer cancel()

	ctx = scope.With(ctx, scope.FromEnvelope(env))

	slog.Info("agent: turn begin",
		"integration", env.Integration, "channel", env.Channel, "user", env.User)
	slog.Debug("agent: message received",
		"integration", env.Integration, "channel", env.Channel,
		"user", env.User, "user_name", env.UserName, "content", env.Content)

	transcript := a.ctxb.ComposeTranscript(env)

	turn := &Turn{
		Ctx:        ctx,
		Env:        env,
		Transcript: transcript,
	}
	a.react(turn, reactReceived)

	// --- Phase 1: plan -------------------------------------------------
	a.react(turn, reactPlanning)
	plan, err := a.planner.Run(ctx, env, transcript)
	if err != nil {
		slog.Error("agent: planner failed; aborting turn",
			"err", err,
			"integration", env.Integration, "channel", env.Channel, "user", env.User,
			"ctx_err", ctx.Err())
		a.deliver(turn)
		return
	}
	turn.Plan = plan
	slog.Info("agent: plan committed",
		"goal", plan.Goal, "steps", len(plan.Steps), "has_tools", plan.HasTools())

	// --- Phase 2: announce --------------------------------------------
	// Send the plan message and capture its ID for in-place edits.
	// Only announce when there's real work to show: a plan of pure
	// respond-only steps (e.g. greetings) gives the user nothing useful
	// in an announcement, so we skip it.
	if plan.HasTools() {
		if msg := plan.RenderAnnouncement(); msg != "" {
			id, sendErr := a.replies.Send(ctx, env.Integration, env.Channel, env.Thread, msg)
			if sendErr != nil {
				slog.Warn("agent: announce failed; continuing without status updates",
					"err", sendErr, "integration", env.Integration)
			} else {
				turn.PlanMessageID = id
			}
		}
	}

	// --- Phase 3: execute ---------------------------------------------
	if plan.HasTools() {
		a.react(turn, reactExecuting)
	}
	for !plan.Complete() {
		if ctx.Err() != nil {
			slog.Warn("agent: turn ctx expired during execution")
			break
		}
		step := plan.Next()
		if step == nil {
			break
		}

		// Mark the step running and push that state to the user.
		step.Status = StepRunning
		a.updatePlanMessage(turn)

		slog.Debug("agent: step begin",
			"id", step.ID, "intent", step.Intent, "tools", step.Tools)
		ok := a.exec.RunStep(turn, step)
		if ok {
			slog.Info("agent: step done", "id", step.ID)
		} else {
			slog.Warn("agent: step failed", "id", step.ID, "err", step.Error)
		}
		a.updatePlanMessage(turn)
	}

	// --- Phase 4: synthesise ------------------------------------------
	if ctx.Err() == nil {
		out, serr := a.synth.Finalize(turn)
		if serr != nil {
			slog.Error("agent: synthesizer failed; aborting reply",
				"err", serr,
				"integration", env.Integration, "channel", env.Channel, "user", env.User)
		} else {
			turn.Reply = strings.TrimSpace(out)
		}
	}

	// --- Phase 5: deliver ---------------------------------------------
	a.deliver(turn)
}

// updatePlanMessage edits the plan-announcement message to reflect
// current step statuses. Best-effort: if there's no message ID (e.g.
// announce was skipped or failed) or the editor isn't available, this
// is a no-op apart from a debug log.
func (a *Agent) updatePlanMessage(t *Turn) {
	if t.PlanMessageID == "" {
		return
	}
	msg := t.Plan.RenderStatus()
	if msg == "" {
		return
	}
	if err := a.replies.Edit(t.Ctx, t.Env.Integration, t.Env.Channel, t.PlanMessageID, msg); err != nil {
		slog.Debug("agent: plan-message edit failed; continuing",
			"err", err, "message_id", t.PlanMessageID)
	}
}

// react adds an emoji reaction to the inbound message and records it on
// the turn so it can be cleared later. Best-effort: no message ID (e.g.
// scheduler ticks), no registered reactor, or a transport error all
// degrade to a debug log — reactions are feedback, never load-bearing.
func (a *Agent) react(t *Turn, emoji string) {
	if t.Env.MessageID == "" {
		return
	}
	if err := a.replies.React(t.Ctx, t.Env.Integration, t.Env.Channel, t.Env.MessageID, emoji); err != nil {
		slog.Debug("agent: react failed; continuing", "err", err, "emoji", emoji)
		return
	}
	t.Reactions = append(t.Reactions, emoji)
}

// clearReactions removes every reaction react added, in order. Used on a
// successful turn so the delivered reply is the only signal left.
func (a *Agent) clearReactions(t *Turn) {
	if t.Env.MessageID == "" {
		return
	}
	for _, emoji := range t.Reactions {
		if err := a.replies.RemoveReaction(t.Ctx, t.Env.Integration, t.Env.Channel, t.Env.MessageID, emoji); err != nil {
			slog.Debug("agent: reaction removal failed; continuing", "err", err, "emoji", emoji)
		}
	}
	t.Reactions = nil
}

// deliver sends the reply (if any). A best-effort operation — a
// failure here must not block future turns.
func (a *Agent) deliver(t *Turn) {
	if t.Reply == "" {
		slog.Warn("agent: turn produced no reply text",
			"integration", t.Env.Integration, "channel", t.Env.Channel,
			"user", t.Env.User)
	} else {
		slog.Info("agent: deliver: sending",
			"integration", t.Env.Integration, "channel", t.Env.Channel,
			"thread", t.Env.Thread, "user", t.Env.User,
			"chars", len(t.Reply), "content", t.Reply)
		if _, err := a.replies.Send(t.Ctx, t.Env.Integration, t.Env.Channel, t.Env.Thread, t.Reply); err != nil {
			slog.Error("agent: deliver failed", "err", err)
		}
	}

	// Terminal reaction: a produced reply is success (clear the trail);
	// an empty reply means the turn failed to answer (mark it). ❌ is
	// applied here and nowhere else — planner/executor/synth handle
	// their own retry loops internally so that by the time we reach
	// this point every retry-worthy failure (protocol violation OR
	// transient LLM error) has already been exhausted. Do not add
	// react(t, reactFailed) calls elsewhere.
	if t.Reply == "" {
		a.react(t, reactFailed)
	} else {
		a.clearReactions(t)
	}

	slog.Info("agent: turn end",
		"integration", t.Env.Integration, "channel", t.Env.Channel, "chars", len(t.Reply))
}
