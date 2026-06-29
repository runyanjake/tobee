package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/tools"
)

// Executor runs one Step at a time through a ReAct sub-loop. The
// planner pre-commits the plan; the executor focuses each step on its
// declared intent and tool set. The model is free to call tools or
// emit terminal text within the step; terminal text becomes the step's
// Result. Mid-step text without tool calls is treated as the step's
// completion, which is the model's "I'm done" signal.
type Executor struct {
	client      *llm.Client
	tools       *tools.Registry
	ctxb        *ContextBuilder
	maxPerStep  int
	totalBudget int
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
// message. Returns true when the step ended cleanly.
//
// A step with no Tools is the planner's wrap-fallback case: Result is
// already set, no LLM call is made, the step is marked done as-is.
func (e *Executor) RunStep(t *Turn, step *Step) bool {
	step.Status = StepRunning
	step.Attempts++

	// Wrap-fallback case: step represents trivial respond-only output.
	// Result was pre-populated by the planner; nothing to execute.
	if len(step.Tools) == 0 {
		step.Status = StepDone
		return true
	}

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

		logPrompt("agent: executor: prompt", callMsgs)
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
			"finish", resp.Finish,
			"text_chars", len(resp.Text),
			"text", resp.Text,
			"tool_calls_count", len(resp.ToolCalls),
			"tool_calls", renderToolCalls(resp.ToolCalls))

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
// step. Only the planner-listed tools that exist in the registry are
// advertised; missing tools are dropped with a warning, an empty
// intersection is a step-time failure.
func (e *Executor) toolsForStep(step *Step) ([]llm.ToolSpec, error) {
	all := e.tools.Specs()
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

// composeStepSystem renders the executor system message: persona +
// data sections + plan + a <current_step> reminder scoped to the
// running step.
func (e *Executor) composeStepSystem(t *Turn, step *Step) string {
	sys := e.ctxb.ComposeSystem(t.Env, e.ctxb.Persona)
	if r := t.Plan.Render(); r != "" {
		sys += "\n\n" + r
	}
	sys += fmt.Sprintf(`

<current_step id="%s">
You are executing step %s of %d: %s

Respond with exactly one tool call OR a brief terminal text describing
the result (no tool calls) to mark the step complete. Plain prose
mid-step without a tool call ends the step.
</current_step>`, step.ID, step.ID, len(t.Plan.Steps), step.Intent)
	return sys
}
