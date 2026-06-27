package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/tools"
)

// Config controls the agent's execution limits.
type Config struct {
	MaxSteps   int           // hard cap on tool-call iterations per turn
	TurnBudget time.Duration // wall-clock cap on a single turn
}

// Agent is the serial worker that consumes envelopes from the bus and drives
// each through a ReAct-style tool-use loop.
type Agent struct {
	bus      *integrations.Bus
	llm      *llm.Client
	tools    *tools.Registry
	sessions *SessionStore
	ctxb     *ContextBuilder
	replies  *Replies
	summ     *Summarizer

	cfg Config
}

func New(
	bus *integrations.Bus,
	client *llm.Client,
	reg *tools.Registry,
	sessions *SessionStore,
	ctxb *ContextBuilder,
	replies *Replies,
	summ *Summarizer,
	cfg Config,
) *Agent {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	if cfg.TurnBudget <= 0 {
		cfg.TurnBudget = 2 * time.Minute
	}
	return &Agent{
		bus: bus, llm: client, tools: reg, sessions: sessions,
		ctxb: ctxb, replies: replies, summ: summ, cfg: cfg,
	}
}

// Start launches the single worker goroutine. It returns immediately; cancel
// ctx to stop the loop. One goroutine processes envelopes sequentially so
// memory-file writes never race and replies remain deterministic.
func (a *Agent) Start(ctx context.Context) {
	go func() {
		slog.Info("agent: loop started", "tools", a.tools.Names(), "senders", a.replies.Summary())
		for {
			select {
			case <-ctx.Done():
				slog.Info("agent: loop stopped")
				return
			case env := <-a.bus.C():
				a.processTurn(ctx, env)
			}
		}
	}()
}

// processTurn drives a single envelope to completion.
func (a *Agent) processTurn(parent context.Context, env integrations.Envelope) {
	ctx, cancel := context.WithTimeout(parent, a.cfg.TurnBudget)
	defer cancel()

	// Attach the originating user identity so memory tools and reporters
	// can route to the right slice of state. Scheduler ticks carry an empty
	// User and tools that require user scope will surface a clear error.
	ctx = scope.With(ctx, scope.FromEnvelope(env))

	slog.Info("agent: turn begin",
		"integration", env.Integration, "channel", env.Channel, "user", env.User)

	session := a.sessions.Get(env.Key(), env.IsDirect)
	messages := a.ctxb.Build(env)

	// The user message is the last entry Build produced; commit it to the
	// session transcript immediately so it survives a crash mid-turn.
	session.Append(llm.Message{Role: llm.RoleUser, Content: env.Content})

	toolSpecs := a.tools.Specs()
	finalText := ""

	for step := 0; step < a.cfg.MaxSteps; step++ {
		resp, err := a.llm.Call(ctx, messages, toolSpecs)
		if err != nil {
			slog.Error("agent: llm error", "step", step, "err", err)
			return
		}

		// Record the assistant message (text + any tool_calls) verbatim so
		// the transcript matches what we send back on the next iteration.
		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, asst)
		session.Append(asst)

		if resp.Text != "" {
			finalText = resp.Text
		}

		if len(resp.ToolCalls) == 0 {
			break
		}

		for _, tc := range resp.ToolCalls {
			out, cerr := a.tools.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			content := out
			if cerr != nil {
				slog.Warn("agent: tool error", "tool", tc.Function.Name, "err", cerr)
				content = fmt.Sprintf("error: %v", cerr)
			} else {
				slog.Debug("agent: tool ok", "tool", tc.Function.Name)
			}
			tmsg := llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    content,
			}
			messages = append(messages, tmsg)
			session.Append(tmsg)
		}
	}

	if finalText != "" {
		a.replies.Send(ctx, env.Integration, env.Channel, env.Thread, finalText)
	} else {
		slog.Debug("agent: turn produced no reply text")
	}

	// Rolling summary is best-effort; failure must not block future turns.
	if a.summ != nil {
		if err := a.summ.Update(ctx, env.Key(), session.Recent()); err != nil {
			slog.Warn("agent: summarizer failed", "err", err)
		}
	}

	slog.Info("agent: turn end",
		"integration", env.Integration, "channel", env.Channel, "chars", len(finalText))
}
