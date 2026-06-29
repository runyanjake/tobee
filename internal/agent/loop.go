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
// level. The per-turn iteration cap lives on Executor.
type Config struct {
	TurnBudget time.Duration // wall-clock cap on a single turn
}

// Agent is the serial worker that consumes envelopes from the bus and
// drives each through a uniform two-phase turn: a ReAct loop (think +
// act + reflect, driven by the model itself) followed by a synthesis
// LLM call that composes the user-facing reply. One goroutine processes
// envelopes sequentially.
type Agent struct {
	bus      *integrations.Bus
	sessions *SessionStore
	ctxb     *ContextBuilder
	replies  *Replies
	summ     *Summarizer

	exec  *Executor
	synth *Synthesizer

	cfg Config
}

func New(
	bus *integrations.Bus,
	sessions *SessionStore,
	ctxb *ContextBuilder,
	replies *Replies,
	summ *Summarizer,
	exec *Executor,
	synth *Synthesizer,
	cfg Config,
) *Agent {
	if cfg.TurnBudget <= 0 {
		cfg.TurnBudget = 2 * time.Minute
	}
	return &Agent{
		bus: bus, sessions: sessions, ctxb: ctxb,
		replies: replies, summ: summ,
		exec: exec, synth: synth,
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

// processTurn drives one envelope through:
//
//  1. ReAct loop (Executor.Run): the model is given the persona, data
//     sections, recent transcript, and all registered tools, and loops
//     calling tools until it produces terminal text (no tool calls).
//     Zero tool calls is a valid first-iteration outcome — e.g. for
//     greetings, the model just says hello and the loop terminates
//     immediately.
//  2. Synthesis (Synthesizer.Finalize): always runs. Reads the
//     transcript and composes the user-facing reply in tobee's voice.
//     This is the place where tone, formatting, and length are
//     enforced — the act loop is free to be scratchpad-ish.
//  3. Deliver: send the reply to the originating integration, mirror
//     it into the session, run the (best-effort) summarizer.
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

	if ok := a.exec.Run(turn); !ok {
		slog.Warn("agent: act loop hit budget without natural termination")
	}

	if ctx.Err() == nil {
		out, err := a.synth.Finalize(turn)
		if err != nil {
			slog.Error("agent: synthesizer failed; falling back to last assistant text",
				"err", err,
				"integration", env.Integration, "channel", env.Channel, "user", env.User)
			turn.Reply = strings.TrimSpace(lastAssistantText(turn.Transcript))
		} else {
			turn.Reply = strings.TrimSpace(out)
		}
	}

	a.deliver(turn)
}

// lastAssistantText returns the most recent assistant message's text
// from the transcript, used as a fallback reply when synthesis fails.
func lastAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// deliver sends the reply (if any) and runs the post-turn summarizer.
// Both are best-effort: a failure here must not block future turns.
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
		// Mirror the delivered reply into the session so the next turn's
		// model sees what was said. Synthesised replies are otherwise
		// absent from the act-loop transcript.
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
