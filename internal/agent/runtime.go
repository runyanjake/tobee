package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/taskqueue"
	"github.com/runyanjake/tobee/internal/telemetry"
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

// Config controls the runtime's turn-level limits. Per-step and total
// executor caps live on Executor.
type Config struct {
	TurnBudget time.Duration // wall-clock cap on a single turn
}

// Runtime is the serial worker: it takes one task at a time off the
// queue, runs it through the strategy, and delivers or parks the result
// (D-005). There is no state across tasks except parked questions.
type Runtime struct {
	queue    *taskqueue.Queue
	ctxb     *ContextBuilder
	out      *delivery.Router
	strategy Strategy
	cfg      Config
}

func NewRuntime(queue *taskqueue.Queue, ctxb *ContextBuilder, out *delivery.Router, strategy Strategy, cfg Config) *Runtime {
	if cfg.TurnBudget <= 0 {
		cfg.TurnBudget = 2 * time.Minute
	}
	return &Runtime{queue: queue, ctxb: ctxb, out: out, strategy: strategy, cfg: cfg}
}

// Start launches the single worker goroutine. Cancel ctx to stop.
func (r *Runtime) Start(ctx context.Context) {
	go func() {
		slog.Info("agent: runtime started", "strategy", r.strategy.Name(), "channels", r.out.Names())
		for {
			task, err := r.queue.Next(ctx)
			if err != nil {
				slog.Info("agent: runtime stopped")
				return
			}
			r.run(ctx, task)
		}
	}()
}

func (r *Runtime) run(parent context.Context, task *taskqueue.Task) {
	ctx, cancel := context.WithTimeout(parent, r.cfg.TurnBudget)
	defer cancel()

	ev := task.Event
	ctx = scope.With(ctx, scope.FromEvent(ev))
	// Every record from here to turn end carries the task ID (D-040).
	ctx = telemetry.With(ctx, "task", task.ID)
	log := telemetry.Logger(ctx)

	log.Info("agent: turn begin", "attempt", task.Attempts)
	inputAttrs := []any{
		"source", ev.Source, "kind", ev.Kind,
		"connector", ev.Origin.Connector, "channel", ev.Origin.Channel,
		"user", ev.Actor.ID, "user_name", ev.Actor.Name,
		telemetry.Content("content", ev.Content),
	}
	if res := task.Resume; res != nil {
		inputAttrs = append(inputAttrs, "resumes", res.TaskID,
			telemetry.Content("request", res.Request), telemetry.Content("question", res.Question))
	}
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Input, "agent: input", inputAttrs...)

	turn := &Turn{
		Ctx:          ctx,
		Task:         task,
		Event:        ev,
		Conversation: NewConversation(r.ctxb.ComposeSystem(ev)),
		out:          r.out,
	}
	turn.React(reactReceived)

	r.strategy.Handle(turn)

	if turn.Await != nil {
		r.park(turn)
		return
	}
	r.deliver(turn)
	r.queue.Done(task)
}

// park records the task as waiting on the user's answer. The question
// was already sent by the tool that asked it; it is logged here as the
// turn's output so every turn ends with one output record.
func (r *Runtime) park(t *Turn) {
	telemetry.Log(t.Ctx, slog.LevelInfo, telemetry.Output, "agent: output",
		"kind", "question", "connector", t.Event.Origin.Connector, "channel", t.Event.Origin.Channel,
		telemetry.Content("content", t.Await.Question))
	if err := r.queue.Park(t.Task, t.Await.Question, t.Await.Keys); err != nil {
		telemetry.Logger(t.Ctx).Error("agent: park failed; the answer will start a fresh task", "err", err)
		r.queue.Done(t.Task)
	}
	t.clearReactions()
	telemetry.Logger(t.Ctx).Info("agent: turn parked awaiting user", "keys", len(t.Await.Keys))
}

// deliver sends the reply (if any). A best-effort operation — a
// failure here must not block future turns.
func (r *Runtime) deliver(t *Turn) {
	ev := t.Event
	log := telemetry.Logger(t.Ctx)
	if t.Reply == "" {
		log.Warn("agent: turn produced no reply text")
	} else {
		telemetry.Log(t.Ctx, slog.LevelInfo, telemetry.Output, "agent: output",
			"kind", "reply", "connector", ev.Origin.Connector, "channel", ev.Origin.Channel,
			"thread", ev.Origin.Thread, telemetry.Content("content", t.Reply))
		if _, err := r.out.Send(t.Ctx, ev.Origin, t.Reply); err != nil {
			log.Error("agent: deliver failed", "err", err)
		}
	}

	// Terminal reaction: a produced reply is success (clear the trail);
	// an empty reply means the turn failed to answer (mark it). ❌ is
	// applied here and nowhere else — each phase handles its own retries,
	// so by the time we get here every retry-worthy failure has already
	// been exhausted.
	if t.Reply == "" {
		t.React(reactFailed)
	} else {
		t.clearReactions()
	}

	log.Info("agent: turn end", "replied", t.Reply != "")
}
