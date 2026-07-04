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
// drives each through plan → announce → execute → synth → deliver as
// one continuous conversation (D-029). One goroutine processes
// envelopes sequentially; there is no cross-envelope state.
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

// processTurn drives one envelope through a single conversation:
//
//  1. Seed the Conversation with the system message and append the
//     rendered plan-state user message. Planner.Run calls plan.commit
//     and returns a Plan on Turn.Conversation.
//  2. Announce the plan as a Discord message (edited in place as
//     steps run).
//  3. For each step: append the rendered execute_step user message,
//     run the ReAct sub-loop against the same Conversation. If any
//     step.finish sets finished=true, remaining steps are skipped.
//  4. Append the synthesize state message; Synthesizer.Finalize
//     returns the rendered reply.
//  5. Deliver: send the reply, resolve reactions, done.
func (a *Agent) processTurn(parent context.Context, env integrations.Envelope) {
	ctx, cancel := context.WithTimeout(parent, a.cfg.TurnBudget)
	defer cancel()

	ctx = scope.With(ctx, scope.FromEnvelope(env))

	slog.Info("agent: turn begin",
		"integration", env.Integration, "channel", env.Channel, "user", env.User)
	slog.Debug("agent: message received",
		"integration", env.Integration, "channel", env.Channel,
		"user", env.User, "user_name", env.UserName, "content", env.Content)

	systemPrompt := a.ctxb.ComposeSystem(env)
	conv := NewConversation(systemPrompt)

	turn := &Turn{
		Ctx:          ctx,
		Env:          env,
		Conversation: conv,
	}
	a.react(turn, reactReceived)

	// --- Phase 1: plan -------------------------------------------------
	a.react(turn, reactPlanning)
	if err := a.planner.Run(ctx, conv, env.Content); err != nil {
		slog.Error("agent: planner failed; aborting turn",
			"err", err,
			"integration", env.Integration, "channel", env.Channel, "user", env.User,
			"ctx_err", ctx.Err())
		a.deliver(turn)
		return
	}
	plan := conv.Plan
	slog.Info("agent: plan committed",
		"goal", plan.Goal, "steps", len(plan.Steps))

	// --- Phase 2: announce --------------------------------------------
	if msg := plan.RenderAnnouncement(); msg != "" {
		id, sendErr := a.replies.Send(ctx, env.Integration, env.Channel, env.Thread, msg)
		if sendErr != nil {
			slog.Warn("agent: announce failed; continuing without status updates",
				"err", sendErr, "integration", env.Integration)
		} else {
			turn.PlanMessageID = id
		}
	}

	// --- Phase 3: execute ---------------------------------------------
	a.react(turn, reactExecuting)
	stepTotal := len(plan.Steps)
	for i := range plan.Steps {
		if ctx.Err() != nil {
			slog.Warn("agent: turn ctx expired during execution")
			break
		}
		step := &plan.Steps[i]
		conv.StepCursor = i

		step.Status = StepRunning
		a.updatePlanMessage(turn)

		slog.Debug("agent: step begin",
			"id", step.ID, "intent", step.Intent, "num", i+1, "of", stepTotal)
		ok := a.exec.RunStep(turn, i+1, stepTotal, step)
		if ok {
			slog.Info("agent: step done", "id", step.ID, "finished", step.Finished)
		} else {
			slog.Warn("agent: step failed", "id", step.ID, "err", step.Error)
		}
		a.updatePlanMessage(turn)

		if conv.Finished {
			slog.Info("agent: LLM signalled finished=true; skipping remaining steps",
				"at_step", step.ID, "remaining", stepTotal-(i+1))
			for j := i + 1; j < stepTotal; j++ {
				plan.Steps[j].Status = StepSkipped
			}
			a.updatePlanMessage(turn)
			break
		}
	}

	// Correctness surface: if the plan ran to the last step but no
	// step ever set finished=true, log the mismatch. Not fatal — synth
	// still runs.
	if !conv.Finished && plan.Complete() && stepTotal > 0 {
		lastStatus := plan.Steps[stepTotal-1].Status
		if lastStatus == StepDone {
			slog.Warn("agent: last step done without finished=true attestation",
				"last_step", plan.Steps[stepTotal-1].ID)
		}
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
	plan := t.Plan()
	if plan == nil {
		return
	}
	msg := plan.RenderStatus()
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
