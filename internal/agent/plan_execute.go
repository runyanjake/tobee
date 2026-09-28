package agent

import (
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// PlanExecute is the plan → announce → execute → synthesize strategy
// (D-024, D-029), with the planner's direct-reply fast path (D-032).
type PlanExecute struct {
	planner *Planner
	exec    *Executor
	synth   *Synthesizer
	out     *delivery.Router
}

func NewPlanExecute(planner *Planner, exec *Executor, synth *Synthesizer, out *delivery.Router) *PlanExecute {
	return &PlanExecute{planner: planner, exec: exec, synth: synth, out: out}
}

// Name implements Strategy.
func (s *PlanExecute) Name() string { return "plan_execute" }

// Handle drives one turn through a single conversation:
//
//  1. Plan: Planner.Run calls plan_commit and sets the Plan.
//  2. Announce the plan where the connector can edit it afterwards.
//  3. For each step, run the ReAct sub-loop against the same
//     Conversation. finished=true skips the remaining steps; a user_ask
//     ends the turn so the runtime can park it.
//  4. Synthesize the reply via reply_commit.
func (s *PlanExecute) Handle(t *Turn) {
	ctx := t.Ctx
	ev := t.Event
	conv := t.Conversation
	log := telemetry.Logger(ctx)

	// --- Phase 1: plan -------------------------------------------------
	t.React(reactPlanning)
	if err := s.planner.Run(ctx, conv, t.Request()); err != nil {
		log.Error("agent: planner failed; aborting turn", "err", err, "ctx_err", ctx.Err())
		return
	}
	plan := conv.Plan

	// Fast path (D-032): the planner answered outright, so there is
	// nothing to announce, execute, or synthesise.
	if plan.DirectReply != "" {
		t.Reply = plan.DirectReply
		return
	}

	// --- Phase 2: announce --------------------------------------------
	// Only where the message can be edited as steps progress: on email a
	// checklist would be one more message nobody can update.
	if s.out.CanEdit(ev.Origin.Connector) {
		if msg := plan.RenderAnnouncement(); msg != "" {
			telemetry.Log(ctx, slog.LevelInfo, telemetry.Output, "agent: output",
				"kind", "plan", "connector", ev.Origin.Connector, "channel", ev.Origin.Channel,
				telemetry.Content("content", msg))
			id, err := s.out.Send(ctx, ev.Origin, msg)
			if err != nil {
				log.Warn("agent: announce failed; continuing without status updates", "err", err)
			} else {
				t.PlanMessageID = id
			}
		}
	}

	// --- Phase 3: execute ---------------------------------------------
	t.React(reactExecuting)
	stepTotal := len(plan.Steps)
	for i := range plan.Steps {
		if ctx.Err() != nil {
			log.Warn("agent: turn ctx expired during execution")
			break
		}
		step := &plan.Steps[i]
		conv.StepCursor = i

		step.Status = StepRunning
		s.updatePlanMessage(t)

		if !s.exec.RunStep(t, i+1, stepTotal, step) {
			telemetry.Log(ctx, slog.LevelWarn, telemetry.Thinking, "agent: step failed",
				"step", step.ID, "err", step.Error)
		}
		s.updatePlanMessage(t)

		if conv.Finished {
			log.Info("agent: step ended the turn; skipping remaining steps",
				"at_step", step.ID, "remaining", stepTotal-(i+1), "awaiting_user", t.Await != nil)
			for j := i + 1; j < stepTotal; j++ {
				plan.Steps[j].Status = StepSkipped
			}
			s.updatePlanMessage(t)
			break
		}
	}

	// The question is the turn's output; there is nothing to synthesise
	// until the user answers.
	if t.Await != nil {
		return
	}

	// Correctness surface: if the plan ran to the last step but no
	// step ever set finished=true, log the mismatch. Not fatal — synth
	// still runs.
	if !conv.Finished && plan.Complete() && stepTotal > 0 {
		if plan.Steps[stepTotal-1].Status == StepDone {
			log.Warn("agent: last step done without finished=true attestation",
				"last_step", plan.Steps[stepTotal-1].ID)
		}
	}

	// --- Phase 4: synthesise ------------------------------------------
	if ctx.Err() != nil {
		return
	}
	out, err := s.synth.Finalize(t)
	switch {
	case err == nil:
		t.Reply = strings.TrimSpace(out)

	// A tool already rendered the answer, so a dead synthesiser is no
	// reason to send the user nothing. Code owns these blocks (D-030),
	// which is exactly what makes delivering them here safe.
	case len(t.Verbatim) > 0:
		t.Reply = strings.TrimSpace(renderReply(replyCommitArgs{}, t.Verbatim))
		log.Warn("agent: synthesizer failed; delivering verbatim tool output instead",
			"err", err, "blocks", len(t.Verbatim))

	default:
		log.Error("agent: synthesizer failed; aborting reply", "err", err)
	}
}

// updatePlanMessage edits the plan announcement to reflect current step
// statuses. Best-effort: no announcement or a failed edit is a debug log.
func (s *PlanExecute) updatePlanMessage(t *Turn) {
	if t.PlanMessageID == "" {
		return
	}
	msg := t.Plan().RenderStatus()
	if msg == "" {
		return
	}
	if err := s.out.Edit(t.Ctx, t.Event.Origin, t.PlanMessageID, msg); err != nil {
		telemetry.Logger(t.Ctx).Debug("agent: plan-message edit failed; continuing",
			"err", err, "message_id", t.PlanMessageID)
	}
}
