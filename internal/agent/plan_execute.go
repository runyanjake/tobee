package agent

import (
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// PlanExecute is the plan → announce → execute → synthesize strategy (D-024, D-029, D-032).
type PlanExecute struct {
	planner *Planner
	exec    *Executor
	synth   *Synthesizer
	out     *delivery.Router
}

func NewPlanExecute(planner *Planner, exec *Executor, synth *Synthesizer, out *delivery.Router) *PlanExecute {
	return &PlanExecute{planner: planner, exec: exec, synth: synth, out: out}
}

func (s *PlanExecute) Name() string { return "plan_execute" }

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

	// Fast path (D-032): the planner answered outright.
	if plan.DirectReply != "" {
		t.Reply = plan.DirectReply
		return
	}

	// --- Phase 2: announce --------------------------------------------
	// Only where the message is editable; on email a checklist can't be updated.
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

	// The question is the turn's output; nothing to synthesise until answered.
	if t.Await != nil {
		return
	}

	// No step set finished=true: log the mismatch, but synth still runs.
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

	// Tools already rendered the answer; code owns these blocks (D-030), so deliver them.
	case len(t.Verbatim) > 0:
		t.Reply = strings.TrimSpace(renderReply(replyCommitArgs{}, t.Verbatim))
		log.Warn("agent: synthesizer failed; delivering verbatim tool output instead",
			"err", err, "blocks", len(t.Verbatim))

	default:
		log.Error("agent: synthesizer failed; aborting reply", "err", err)
	}
}

// updatePlanMessage is best-effort: a missing announcement or failed edit only logs.
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
