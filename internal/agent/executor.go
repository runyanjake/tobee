package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/tools"
)

// Executor runs a single Step through a ReAct sub-loop. The post-D-001
// ReAct body, scoped to one Step's worth of LLM iterations.
type Executor struct {
	client      *llm.Client
	tools       *tools.Registry
	ctxb        *ContextBuilder
	maxPerStep  int // hard cap on LLM iterations per step
	totalBudget int // hard cap on LLM iterations across the whole turn
}

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

// RunStep executes one Step. Mutates step in place (Status, Result,
// Error, Attempts) and grows t.Transcript with every assistant + tool
// message. Returns true when the step ended cleanly (Status == StepDone).
// Returns false when the step failed — caller decides whether to replan.
func (e *Executor) RunStep(t *Turn, step *Step) bool {
	step.Status = StepRunning
	step.Attempts++

	toolSpecs, terr := e.toolsForStep(step)
	if terr != nil {
		step.Error = terr.Error()
		step.Status = StepFailed
		return false
	}
	stepResult := ""
	clean := false

	for sub := 0; sub < e.maxPerStep; sub++ {
		if t.Plan.StepsRun >= e.totalBudget {
			step.Error = "turn step-budget exhausted"
			step.Status = StepFailed
			slog.Warn("agent: executor: total step budget exhausted",
				"step", step.ID, "steps_run", t.Plan.StepsRun, "total_budget", e.totalBudget)
			return false
		}
		t.Plan.StepsRun++

		slog.Debug("agent: executor: iteration",
			"step", step.ID, "sub", sub, "steps_run", t.Plan.StepsRun,
			"transcript_msgs", len(t.Transcript))

		sys := e.composeStepSystem(t, step)
		callMsgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, t.Transcript...)

		resp, err := e.client.Call(t.Ctx, callMsgs, toolSpecs, llm.ToolChoiceUnset)
		if err != nil {
			slog.Error("agent: executor llm error",
				"step", step.ID, "sub", sub, "err", err,
				"integration", t.Env.Integration, "channel", t.Env.Channel, "user", t.Env.User,
				"ctx_err", t.Ctx.Err())
			step.Error = err.Error()
			step.Status = StepFailed
			return false
		}
		slog.Debug("agent: executor: llm response",
			"step", step.ID, "sub", sub,
			"finish", resp.Finish, "text_chars", len(resp.Text),
			"tool_calls", len(resp.ToolCalls))

		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		t.Transcript = append(t.Transcript, asst)
		t.Session.Append(asst)

		if resp.Text != "" {
			stepResult = resp.Text
		}

		if len(resp.ToolCalls) == 0 {
			clean = true
			break
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

	if !clean {
		step.Status = StepFailed
		if step.Error == "" {
			step.Error = fmt.Sprintf("step did not terminate within %d iterations", e.maxPerStep)
		}
		return false
	}

	step.Status = StepDone
	step.Result = strings.TrimSpace(stepResult)
	return true
}

// toolsForStep returns the tool specs advertised to the LLM for this
// step. When the planner listed step.Tools, only the named tools that
// also exist in the registry are advertised; an empty intersection is a
// plan-time error reported back so the loop can replan. When the planner
// did not list any (legacy / minimal plans), all registered tools are
// advertised.
func (e *Executor) toolsForStep(step *Step) ([]llm.ToolSpec, error) {
	all := e.tools.Specs()
	if len(step.Tools) == 0 {
		return all, nil
	}
	wanted := make(map[string]bool, len(step.Tools))
	for _, t := range step.Tools {
		wanted[t] = true
	}
	out := make([]llm.ToolSpec, 0, len(step.Tools))
	for _, s := range all {
		if wanted[s.Name] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("planner listed no known tools: %v", step.Tools)
	}
	if len(out) < len(step.Tools) {
		have := make(map[string]bool, len(out))
		for _, s := range out {
			have[s.Name] = true
		}
		missing := make([]string, 0)
		for _, t := range step.Tools {
			if !have[t] {
				missing = append(missing, t)
			}
		}
		slog.Warn("agent: executor dropped unknown planner tools",
			"step", step.ID, "missing", missing)
	}
	return out, nil
}

// composeStepSystem renders the executor system message for one
// sub-iteration: executor persona + data sections + plan + a focused
// <current_step> reminder pointing at the step that's running.
func (e *Executor) composeStepSystem(t *Turn, step *Step) string {
	sys := e.ctxb.ComposeSystem(t.Env, e.ctxb.Persona, t.Plan)
	sys += fmt.Sprintf(`

<current_step id="%s">
Work on step %s: %s
Use tools as needed. When the step is complete, produce a brief result
as your final message with no tool calls.
</current_step>`, step.ID, step.ID, step.Intent)
	return sys
}
