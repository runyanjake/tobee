package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/tools"
)

// Config controls the agent's execution limits at the turn-coordinator
// level. Per-step and total executor budgets live on Executor.
type Config struct {
	TurnBudget time.Duration // wall-clock cap on a single turn
	MaxReplans int           // hard cap on planner.Revise calls per turn
}

// Agent is the serial worker that consumes envelopes from the bus and
// drives each through the triage → (respond | status | plan/exec/replan/synth)
// state machine. One goroutine processes envelopes sequentially.
type Agent struct {
	bus      *integrations.Bus
	sessions *SessionStore
	ctxb     *ContextBuilder
	replies  *Replies
	summ     *Summarizer
	registry *tools.Registry

	triage  *Triage
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
	registry *tools.Registry,
	triage *Triage,
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
		replies: replies, summ: summ, registry: registry,
		triage: triage, planner: planner, exec: exec, synth: synth,
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
// agent state machine. Each phase is one phaseFn that mutates *Turn and
// returns the next phase to run, or nil to terminate. The state graph:
//
//	phaseTriage ─┬─► (respond) ─► phaseRespond ─► nil
//	             ├─► (status)  ─► phaseStatusDispatch ─► nil
//	             └─► (plan)    ─► phaseExec ─┬─► phaseReplan ─► phaseExec
//	                                         └─► phaseSynth  ─► nil
//
// After the machine terminates, deliver runs the (best-effort) summarizer
// and sends turn.Reply.
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

	turn := &Turn{
		Ctx:        ctx,
		Env:        env,
		Session:    session,
		Transcript: transcript,
	}

	for phase := a.phaseTriage; phase != nil; {
		if turn.Ctx.Err() != nil {
			slog.Warn("agent: turn ctx expired")
			break
		}
		next, err := phase(turn)
		if err != nil {
			slog.Error("agent: phase failed; aborting turn",
				"err", err,
				"integration", env.Integration, "channel", env.Channel, "user", env.User,
				"ctx_err", turn.Ctx.Err())
			break
		}
		phase = next
	}

	a.deliver(turn)
}

// phaseTriage runs the triage LLM call and routes based on the result.
func (a *Agent) phaseTriage(t *Turn) (phaseFn, error) {
	slog.Debug("agent: triage: begin",
		"integration", t.Env.Integration, "channel", t.Env.Channel, "user", t.Env.User,
		"transcript_msgs", len(t.Transcript))

	res, err := a.triage.Run(t.Ctx, t.Env, t.Transcript)
	if err != nil {
		return nil, err
	}
	t.Triage = res
	slog.Info("agent: triage committed", "category", res.Category)

	switch res.Category {
	case TriageCategoryRespond:
		return a.phaseRespond, nil
	case TriageCategoryStatus:
		return a.phaseStatusDispatch, nil
	case TriageCategoryPlan:
		t.Plan = res.Plan
		slog.Info("agent: plan committed",
			"goal", t.Plan.Goal, "steps", len(t.Plan.Steps))

		// Announce multi-step plans so the user sees what's coming while
		// the executor runs. Mirrored into the session so the next turn's
		// triage sees what the user was told.
		if t.Plan.HasMultipleSteps() {
			if msg := t.Plan.RenderUserMessage(); msg != "" {
				a.replies.Send(t.Ctx, t.Env.Integration, t.Env.Channel, t.Env.Thread, msg)
				t.Session.Append(llm.Message{Role: llm.RoleAssistant, Content: msg})
			}
		}
		return a.phaseExec, nil
	}
	return nil, nil
}

// phaseRespond writes the triage-direct reply onto the turn and ends.
func (a *Agent) phaseRespond(t *Turn) (phaseFn, error) {
	t.Reply = t.Triage.Reply
	slog.Debug("agent: respond", "chars", len(t.Reply))
	return nil, nil
}

// phaseStatusDispatch calls the named status tool directly (no executor
// LLM call) and uses the tool's verbatim output as the reply. D-021 says
// status tools pre-render; passing the output through an LLM only risks
// drift.
func (a *Agent) phaseStatusDispatch(t *Turn) (phaseFn, error) {
	tool := t.Triage.StatusTool
	slog.Debug("agent: status dispatch", "tool", tool)
	out, err := a.registry.Call(t.Ctx, tool, json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	t.Reply = strings.TrimSpace(out)
	return nil, nil
}

// phaseExec runs one step of the committed plan. Returns phaseExec
// again when more steps remain, phaseReplan on step failure (when
// budget allows), or phaseSynth / nil when the plan is done.
func (a *Agent) phaseExec(t *Turn) (phaseFn, error) {
	step := t.Plan.Next()
	if step == nil {
		// Plan complete.
		if t.Plan.HasMultipleSteps() {
			return a.phaseSynth, nil
		}
		t.Reply = strings.TrimSpace(t.Plan.LastStepText())
		return nil, nil
	}

	slog.Debug("agent: step begin",
		"id", step.ID, "intent", step.Intent, "tools", step.Tools)
	ok := a.exec.RunStep(t, step)
	if line := t.Plan.RenderStepStatus(step); line != "" {
		t.Session.Append(llm.Message{Role: llm.RoleAssistant, Content: line})
	}
	if ok {
		slog.Info("agent: step done", "id", step.ID)
		return a.phaseExec, nil
	}

	slog.Warn("agent: step failed", "id", step.ID, "err", step.Error)

	if t.Plan.Replans >= a.cfg.MaxReplans {
		slog.Warn("agent: replan budget exhausted; finalising incomplete plan",
			"replans", t.Plan.Replans)
		if t.Plan.HasMultipleSteps() {
			return a.phaseSynth, nil
		}
		t.Reply = strings.TrimSpace(t.Plan.LastStepText())
		return nil, nil
	}
	return a.phaseReplan, nil
}

// phaseReplan invokes the planner with the prior plan + the failing
// step's error. Successful replan jumps back to phaseExec with the
// revised plan; failure finalises with whatever we have.
func (a *Agent) phaseReplan(t *Turn) (phaseFn, error) {
	// Find the failed step to extract its error for the replan reason.
	var reason string
	for i := range t.Plan.Steps {
		if t.Plan.Steps[i].Status == StepFailed {
			reason = t.Plan.Steps[i].Error
			break
		}
	}

	slog.Debug("agent: phase=replan", "replans_so_far", t.Plan.Replans, "reason", reason)
	revised, err := a.planner.Revise(t.Ctx, t.Env, t.Plan, reason, t.Transcript)
	if err != nil {
		slog.Error("agent: replan failed; finalising incomplete plan",
			"err", err, "replans_so_far", t.Plan.Replans,
			"integration", t.Env.Integration, "channel", t.Env.Channel, "user", t.Env.User)
		if t.Plan.HasMultipleSteps() {
			return a.phaseSynth, nil
		}
		t.Reply = strings.TrimSpace(t.Plan.LastStepText())
		return nil, nil
	}
	slog.Info("agent: plan revised",
		"replans", revised.Replans, "steps", len(revised.Steps))
	t.Plan = revised
	return a.phaseExec, nil
}

// phaseSynth runs the synthesizer over the completed multi-step plan,
// falling back to the last step's text if synthesis fails.
func (a *Agent) phaseSynth(t *Turn) (phaseFn, error) {
	slog.Debug("agent: phase=synthesize",
		"steps_run", t.Plan.StepsRun, "replans", t.Plan.Replans)
	out, err := a.synth.Finalize(t)
	if err != nil {
		slog.Error("agent: synthesizer failed; falling back to last step",
			"err", err,
			"integration", t.Env.Integration, "channel", t.Env.Channel, "user", t.Env.User,
			"steps", len(t.Plan.Steps), "steps_run", t.Plan.StepsRun)
		t.Reply = strings.TrimSpace(t.Plan.LastStepText())
		return nil, nil
	}
	t.Reply = strings.TrimSpace(out)
	return nil, nil
}

// deliver sends the reply (if any) and runs the post-turn summarizer.
// Both are best-effort: a failure here must not block future turns.
func (a *Agent) deliver(t *Turn) {
	if t.Reply == "" {
		slog.Debug("agent: turn produced no reply text")
	} else {
		slog.Debug("agent: message sent",
			"integration", t.Env.Integration, "channel", t.Env.Channel,
			"user", t.Env.User, "content", t.Reply)
		// Mirror the delivered reply into the session so the next turn's
		// triage sees what was said. Synthesised and triage-direct
		// replies are not otherwise present in the executor transcript.
		t.Session.Append(llm.Message{Role: llm.RoleAssistant, Content: t.Reply})
		a.replies.Send(t.Ctx, t.Env.Integration, t.Env.Channel, t.Env.Thread, t.Reply)
	}

	if a.summ != nil {
		slog.Debug("agent: phase=summarize", "key", t.Env.Key())
		if err := a.summ.Update(t.Ctx, t.Env.Key(), t.Session.Recent()); err != nil {
			slog.Warn("agent: summarizer failed",
				"err", err, "key", t.Env.Key())
		}
	}

	slog.Info("agent: turn end",
		"integration", t.Env.Integration, "channel", t.Env.Channel, "chars", len(t.Reply))
}
