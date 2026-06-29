package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/tools"
)

// Executor runs the per-turn ReAct loop: think + act + reflect, all
// driven by the model itself. The model is given every registered tool
// and freely picks which (if any) to call. The loop terminates when
// the model emits a response with no tool calls — including the very
// first call, which is the trivial "no tool needed" path.
type Executor struct {
	client        *llm.Client
	tools         *tools.Registry
	ctxb          *ContextBuilder
	maxIterations int
}

// NewExecutor builds an Executor. maxIterations is the hard cap on LLM
// iterations per turn; non-positive uses the default (12).
func NewExecutor(client *llm.Client, reg *tools.Registry, ctxb *ContextBuilder, maxIterations int) *Executor {
	if maxIterations <= 0 {
		maxIterations = 12
	}
	return &Executor{
		client:        client,
		tools:         reg,
		ctxb:          ctxb,
		maxIterations: maxIterations,
	}
}

// Run drives the ReAct loop. Mutates t.Transcript in place: every
// assistant message and every tool result is appended. Returns true
// when the loop terminated cleanly (model emitted terminal text);
// false when the iteration cap was hit first.
func (e *Executor) Run(t *Turn) bool {
	specs := e.tools.Specs()

	for i := 0; i < e.maxIterations; i++ {
		if t.Ctx.Err() != nil {
			slog.Warn("agent: act: ctx expired", "iter", i)
			return false
		}

		slog.Debug("agent: act: iteration",
			"iter", i, "transcript_msgs", len(t.Transcript))

		sys := e.ctxb.ComposeSystem(t.Env, e.ctxb.Persona)
		callMsgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, t.Transcript...)

		resp, err := e.client.Call(t.Ctx, callMsgs, specs, llm.ToolChoiceUnset)
		if err != nil {
			slog.Error("agent: act: llm error",
				"iter", i, "err", err,
				"integration", t.Env.Integration, "channel", t.Env.Channel, "user", t.Env.User,
				"ctx_err", t.Ctx.Err())
			return false
		}
		slog.Debug("agent: act: llm response",
			"iter", i,
			"finish", resp.Finish, "text_chars", len(resp.Text),
			"tool_calls", len(resp.ToolCalls))

		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		t.Transcript = append(t.Transcript, asst)
		t.Session.Append(asst)

		if len(resp.ToolCalls) == 0 {
			// Terminal state. Either the model said its piece (text),
			// or it produced nothing at all — both end the loop. The
			// synthesizer phase will turn whatever is in the transcript
			// into the user-facing reply.
			return true
		}

		for _, tc := range resp.ToolCalls {
			out, cerr := e.tools.Call(t.Ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
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
			t.Transcript = append(t.Transcript, tmsg)
			t.Session.Append(tmsg)
		}
	}

	slog.Warn("agent: act: iteration cap hit without terminal text",
		"cap", e.maxIterations)
	return false
}
