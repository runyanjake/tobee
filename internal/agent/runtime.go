package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/session"
	"github.com/runyanjake/tobee/internal/taskqueue"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// Progress reactions on the inbound message; a successful turn clears them.
const (
	reactReceived  = "✅"
	reactPlanning  = "🧠"
	reactExecuting = "💭"
	reactFailed    = "❌"
)

// Config holds turn-level limits; the step cap lives on the strategy (Loop).
type Config struct {
	TurnBudget time.Duration // wall-clock cap per turn
}

// Runtime runs one task at a time (D-005); only parked questions persist across tasks.
type Runtime struct {
	queue    *taskqueue.Queue
	ctxb     *ContextBuilder
	out      *delivery.Router
	strategy Strategy
	sessions *session.Store // nil: no history between turns
	cfg      Config
}

func NewRuntime(queue *taskqueue.Queue, ctxb *ContextBuilder, out *delivery.Router, strategy Strategy, cfg Config) *Runtime {
	if cfg.TurnBudget <= 0 {
		cfg.TurnBudget = 2 * time.Minute
	}
	return &Runtime{queue: queue, ctxb: ctxb, out: out, strategy: strategy, cfg: cfg}
}

// WithSessions gives each person's turns the recent conversation (D-046).
func (r *Runtime) WithSessions(s *session.Store) *Runtime {
	r.sessions = s
	return r
}

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
		Conversation: NewConversation(r.ctxb.ComposeSystem(ctx, ev), r.history(ev)),
		out:          r.out,
	}
	turn.React(reactReceived)

	r.strategy.Handle(turn)

	if turn.Await != nil {
		r.park(turn)
		return
	}
	r.deliver(turn)
	r.record(turn)
	r.queue.Done(task)
}

func (r *Runtime) history(ev event.Event) []llm.Message {
	if r.sessions == nil {
		return nil
	}
	return r.sessions.History(ev.Actor.Person, time.Now())
}

// record saves the finished exchange to the person's session. The final
// reply call becomes the text the user actually received, so history holds
// what was delivered, not what the model drafted (D-046). A parked turn is
// recorded when its task finishes, with the question and answer inline.
func (r *Runtime) record(t *Turn) {
	if r.sessions == nil {
		return
	}
	var msgs []llm.Message
	for _, m := range t.Conversation.TurnMessages() {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) == 1 && m.ToolCalls[0].Function.Name == replyTool {
			continue
		}
		msgs = append(msgs, m)
	}
	reply := t.Reply
	if reply == "" {
		reply = "(no reply was sent)"
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: reply})
	r.sessions.Record(t.Event.Actor.Person, session.Exchange{
		At:        time.Now(),
		Connector: t.Event.Origin.Connector,
		Channel:   t.Event.Origin.Channel,
		UserName:  t.Event.Actor.Name,
		Messages:  msgs,
		Outcome:   outcome(t),
	})
}

// outcome is the turn's own account of what it tried and what failed, written
// from the records code kept, never from the model's words (D-051). The
// session carries it into later turns and into the archived transcript.
func outcome(t *Turn) *session.Outcome {
	o := &session.Outcome{Status: "replied", Steps: t.Steps}
	if t.Await != nil {
		o.Status = "parked"
	} else if t.Reply == "" {
		o.Status = "no reply"
	}
	for _, a := range t.Actions {
		mark := "✅"
		if !a.OK {
			mark = "❌"
		}
		o.Acted = append(o.Acted, fmt.Sprintf("%s %s: %s", mark, a.Tool, a.Result))
	}
	for _, p := range t.Problems {
		line := p.Detail
		if p.Recovered {
			line += " (recovered)"
		}
		o.Problems = append(o.Problems, line)
	}
	if !o.Notable() && len(o.Acted) == 0 {
		return nil // a clean turn's messages already say everything
	}
	return o
}

// park logs the already-sent question as the turn's output so every turn ends with one output record.
func (r *Runtime) park(t *Turn) {
	telemetry.Log(t.Ctx, slog.LevelInfo, telemetry.Output, "agent: output",
		"kind", "question", "connector", t.Event.Origin.Connector, "channel", t.Event.Origin.Channel,
		telemetry.Content("content", t.Await.Question))
	if err := r.queue.Park(t.Task, t.Await.Question, t.Await.Keys, t.Pending); err != nil {
		telemetry.Logger(t.Ctx).Error("agent: park failed; the answer will start a fresh task", "err", err)
		r.queue.Done(t.Task)
	}
	t.clearReactions()
	telemetry.Logger(t.Ctx).Info("agent: turn parked awaiting user", "keys", len(t.Await.Keys))
}

// deliver is best-effort: a failure must not block future turns.
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

	// ❌ is applied only here: phases have already exhausted their own retries.
	if t.Reply == "" {
		t.React(reactFailed)
	} else {
		t.clearReactions()
	}

	log.Info("agent: turn end", "replied", t.Reply != "")
}
