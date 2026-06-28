package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/tools"
)

// Executor runs a single Step through a ReAct sub-loop. It is the
// post-D-001 ReAct body extracted from the old monolithic loop, scoped to
// a single Step's worth of LLM iterations.
type Executor struct {
	client      *llm.Client
	tools       *tools.Registry
	ctxb        *ContextBuilder
	maxPerStep  int // hard cap on LLM iterations per step
	totalBudget int // hard cap on LLM iterations across the whole turn
}

// NewExecutor builds an Executor. maxPerStep and totalBudget are hard
// caps; either being non-positive means use sensible defaults (4 and 12).
func NewExecutor(client *llm.Client, reg *tools.Registry, ctxb *ContextBuilder, maxPerStep, totalBudget int) *Executor {
	if maxPerStep <= 0 {
		maxPerStep = 4
	}
	if totalBudget <= 0 {
		totalBudget = 12
	}
	return &Executor{
		client:      client,
		tools:       reg,
		ctxb:        ctxb,
		maxPerStep:  maxPerStep,
		totalBudget: totalBudget,
	}
}

// RunStep executes one Step. Mutates step in place (Status, Result, Error,
// Attempts) and returns the (possibly grown) transcript. Appends every
// assistant and tool message to session as it goes so a mid-turn crash
// resumes with the model's exact partial state.
//
// Returns true when the step ended cleanly (Status == StepDone). Returns
// false when the step failed — caller decides whether to replan.
func (e *Executor) RunStep(
	ctx context.Context,
	env integrations.Envelope,
	plan *Plan,
	step *Step,
	transcript []llm.Message,
	session *Session,
) ([]llm.Message, bool) {
	step.Status = StepRunning
	step.Attempts++

	toolSpecs := e.tools.Specs()
	stepResult := ""
	clean := false

	for sub := 0; sub < e.maxPerStep; sub++ {
		if plan.StepsRun >= e.totalBudget {
			step.Error = "turn step-budget exhausted"
			step.Status = StepFailed
			return transcript, false
		}
		plan.StepsRun++

		sys := e.composeStepSystem(env, plan, step)
		callMsgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

		resp, err := e.client.Call(ctx, callMsgs, toolSpecs)
		if err != nil {
			slog.Error("agent: executor llm error", "step", step.ID, "sub", sub, "err", err)
			step.Error = err.Error()
			step.Status = StepFailed
			return transcript, false
		}

		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		transcript = append(transcript, asst)
		session.Append(asst)

		if resp.Text != "" {
			stepResult = resp.Text
		}

		if len(resp.ToolCalls) == 0 {
			clean = true
			break
		}

		for _, tc := range resp.ToolCalls {
			out, cerr := e.tools.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
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
			transcript = append(transcript, tmsg)
			session.Append(tmsg)
		}
	}

	if !clean {
		step.Status = StepFailed
		if step.Error == "" {
			step.Error = fmt.Sprintf("step did not terminate within %d iterations", e.maxPerStep)
		}
		return transcript, false
	}

	step.Status = StepDone
	step.Result = strings.TrimSpace(stepResult)
	return transcript, true
}

// composeStepSystem renders the executor system message for one
// sub-iteration: the executor persona + data sections + plan + a focused
// <current_step> reminder pointing at the step that's running.
func (e *Executor) composeStepSystem(env integrations.Envelope, plan *Plan, step *Step) string {
	sys := e.ctxb.ComposeSystem(env, e.ctxb.Persona, plan)
	sys += fmt.Sprintf(`

<current_step id="%s">
Work on step %s: %s
Use tools as needed. When the step is complete, produce a brief result
as your final message with no tool calls.
</current_step>`, step.ID, step.ID, step.Intent)
	return sys
}
