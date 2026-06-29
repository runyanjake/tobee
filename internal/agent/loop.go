package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/scope"
)

// Config controls the agent's execution limits at the turn-coordinator
// level. Per-step and total executor budgets live on Executor.
type Config struct {
	TurnBudget time.Duration // wall-clock cap on a single turn
	MaxReplans int           // hard cap on planner.Revise calls per turn
}

// Agent is the serial worker that consumes envelopes from the bus and
// drives each through the think → plan → act → (replan) → synthesize
// pipeline. One goroutine processes envelopes sequentially.
type Agent struct {
	bus      *integrations.Bus
	sessions *SessionStore
	ctxb     *ContextBuilder
	replies  *Replies
	summ     *Summarizer

	planner *Planner
	exec    *Executor
	synth   *Synthesizer

	cfg Config
}

func New(
	bus *integrations.Bus,
	sessions *SessionStore,
	ctxb *ContextBuilder,
	replies *Replies,
	summ *Summarizer,
	planner *Planner,
	exec *Executor,
	synth *Synthesizer,
	cfg Config,
) *Agent {
	if cfg.TurnBudget <= 0 {
		cfg.TurnBudget = 2 * time.Minute
	}
	if cfg.MaxReplans < 0 {
		cfg.MaxReplans = 0
	}
	return &Agent{
		bus: bus, sessions: sessions, ctxb: ctxb,
		replies: replies, summ: summ,
		planner: planner, exec: exec, synth: synth,
		cfg: cfg,
	}
}

// Start launches the single worker goroutine. Cancel ctx to stop the loop.
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

// processTurn drives a single envelope from inbound to reply through the
// think → plan → act → (replan) → synthesize phases. Each phase is a
// distinct LLM call (or sub-loop of calls) with its own persona; the
// transcript carries through unchanged, with the system slot swapped per
// phase.
func (a *Agent) processTurn(parent context.Context, env integrations.Envelope) {
	ctx, cancel := context.WithTimeout(parent, a.cfg.TurnBudget)
	defer cancel()

	ctx = scope.With(ctx, scope.FromEnvelope(env))

	slog.Info("agent: turn begin",
		"integration", env.Integration, "channel", env.Channel, "user", env.User)
	slog.Debug("agent: message received",
		"integration", env.Integration, "channel", env.Channel,
		"user", env.User, "user_name", env.UserName, "content", env.Content)

	session := a.sessions.Get(env.Key(), env.IsDirect)
	transcript := a.ctxb.ComposeTranscript(env)
	slog.Debug("agent: session loaded",
		"key", session.Key, "recent_msgs", len(session.Recent()),
		"transcript_msgs", len(transcript))

	// Commit the user message to the session transcript before the first
	// LLM call so a crash mid-turn leaves a recoverable record.
	session.Append(llm.Message{Role: llm.RoleUser, Content: env.Content})

	// --- Phase 1: think + plan ------------------------------------------
	slog.Debug("agent: planner: begin",
		"integration", env.Integration, "channel", env.Channel, "user", env.User,
		"transcript_msgs", len(transcript))
	plan, fastReply, err := a.planner.Initial(ctx, env, transcript)
	if err != nil {
		slog.Error("agent: planner failed",
			"err", err,
			"integration", env.Integration, "channel", env.Channel,
			"user", env.User, "user_name", env.UserName,
			"content_chars", len(env.Content),
			"transcript_msgs", len(transcript),
			"ctx_err", ctx.Err())
		return
	}
	if plan == nil {
		// Trivial-turn fast path: the planner replied directly.
		fastReply = strings.TrimSpace(fastReply)
		if fastReply == "" {
			slog.Debug("agent: planner produced empty reply")
			return
		}
		slog.Debug("agent: planner: fast reply", "chars", len(fastReply))
		a.deliver(ctx, env, session, fastReply)
		return
	}

	slog.Info("agent: plan committed",
		"goal", plan.Goal, "steps", len(plan.Steps))
	slog.Debug("agent: phase=execute", "steps", len(plan.Steps), "max_replans", a.cfg.MaxReplans)

	// Announce multi-step plans so the user sees what's coming while the
	// executor runs. Mirrored into the session so the model sees on the
	// next turn what it told the user it would do.
	if plan.HasMultipleSteps() {
		if msg := plan.RenderUserMessage(); msg != "" {
			a.replies.Send(ctx, env.Integration, env.Channel, env.Thread, msg)
			session.Append(llm.Message{Role: llm.RoleAssistant, Content: msg})
		}
	}

	// --- Phase 2: act (per step) with closed-loop replan ---------------
	for !plan.Complete() {
		if ctx.Err() != nil {
			slog.Warn("agent: turn ctx expired during execution")
			break
		}
		step := plan.Next()
		if step == nil {
			break
		}

		slog.Debug("agent: step begin",
			"id", step.ID, "intent", step.Intent, "tools", step.Tools)
		var ok bool
		transcript, ok = a.exec.RunStep(ctx, env, plan, step, transcript, session)
		if line := plan.RenderStepStatus(step); line != "" {
			session.Append(llm.Message{Role: llm.RoleAssistant, Content: line})
		}
		if ok {
			slog.Info("agent: step done", "id", step.ID)
			continue
		}

		slog.Warn("agent: step failed", "id", step.ID, "err", step.Error)

		if plan.Replans >= a.cfg.MaxReplans {
			slog.Warn("agent: replan budget exhausted; finalising incomplete plan",
				"replans", plan.Replans)
			break
		}

		// Phase 2b: replan with the failure context the executor produced.
		slog.Debug("agent: phase=replan",
			"id", step.ID, "replans_so_far", plan.Replans, "reason", step.Error)
		revised, rerr := a.planner.Revise(ctx, env, plan, step.Error, transcript)
		if rerr != nil {
			slog.Error("agent: replan failed; finalising incomplete plan",
				"err", rerr,
				"step", step.ID, "replans_so_far", plan.Replans,
				"integration", env.Integration, "channel", env.Channel, "user", env.User)
			break
		}
		slog.Info("agent: plan revised",
			"replans", revised.Replans, "steps", len(revised.Steps))
		plan = revised
	}

	// --- Phase 3: synthesize (multi-step only) --------------------------
	var reply string
	if plan.HasMultipleSteps() {
		slog.Debug("agent: phase=synthesize", "steps_run", plan.StepsRun, "replans", plan.Replans)
		out, serr := a.synth.Finalize(ctx, env, plan, transcript)
		if serr != nil {
			slog.Error("agent: synthesizer failed; falling back to last step",
				"err", serr,
				"integration", env.Integration, "channel", env.Channel, "user", env.User,
				"steps", len(plan.Steps), "steps_run", plan.StepsRun)
			reply = plan.LastStepText()
		} else {
			reply = out
		}
	} else {
		reply = plan.LastStepText()
	}

	a.deliver(ctx, env, session, strings.TrimSpace(reply))
}

// deliver sends the reply and runs the post-turn summarizer. Both are
// best-effort: a failure here must not block future turns.
func (a *Agent) deliver(ctx context.Context, env integrations.Envelope, session *Session, reply string) {
	if reply == "" {
		slog.Debug("agent: turn produced no reply text")
	} else {
		slog.Debug("agent: message sent",
			"integration", env.Integration, "channel", env.Channel,
			"user", env.User, "content", reply)
		// Mirror the delivered reply into session so the model sees what
		// it actually told the user on the next turn. Plan-synthesised
		// replies are not otherwise present in the executor transcript.
		session.Append(llm.Message{Role: llm.RoleAssistant, Content: reply})
		a.replies.Send(ctx, env.Integration, env.Channel, env.Thread, reply)
	}

	if a.summ != nil {
		slog.Debug("agent: phase=summarize", "key", env.Key())
		if err := a.summ.Update(ctx, env.Key(), session.Recent()); err != nil {
			slog.Warn("agent: summarizer failed",
				"err", err, "key", env.Key())
		}
	}

	slog.Info("agent: turn end",
		"integration", env.Integration, "channel", env.Channel, "chars", len(reply))
}
