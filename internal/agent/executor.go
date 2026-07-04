package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/tools"
)

const stepFinishTool = "step.finish"

var stepFinishSchema = json.RawMessage(`{
  "type": "object",
  "required": ["result"],
  "properties": {
    "result": {"type": "string", "description": "One or two sentences stating the outcome of this step, in the exact form the synthesizer will consume. State the answer, not the procedure."}
  }
}`)

// stepFinishArgs is the JSON shape step.finish emits.
type stepFinishArgs struct {
	Result string `json:"result"`
}

// executorNudge is the reminder appended to the transcript after a
// protocol violation inside a step.
const executorNudge = "PROTOCOL VIOLATION: your previous response was neither a tool call nor a step.finish call. You must call exactly one tool per turn. When the step's outcome is known, call step.finish with the result. Free-form text is not accepted. Retry."

// Executor runs one Step at a time through a ReAct sub-loop. The
// planner pre-commits the plan; the executor focuses each step on its
// declared intent and tool set plus the virtual step.finish tool. The
// only legal terminations are step.finish (success) or the step budget
// (failure). Free-form text is a protocol violation: retried once,
// then fails the step.
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
// message. Returns true when the step ended cleanly via step.finish.
//
// A step with no declared Tools is a legitimate respond-only step
// (e.g. greeting): no LLM call, no Result, marked done immediately.
// The synthesizer handles composing the reply from the plan alone.
func (e *Executor) RunStep(t *Turn, step *Step) bool {
	step.Status = StepRunning
	step.Attempts++

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

	violations := 0
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
		resp, err := e.client.Call(t.Ctx, callMsgs, toolSpecs, llm.ToolChoiceRequired)
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

		if len(resp.ToolCalls) == 0 {
			slog.Error("agent: executor: PROTOCOL VIOLATION",
				"step", step.ID, "sub", sub, "violation", violations,
				"expected_tool_or", stepFinishTool,
				"finish", resp.Finish,
				"text_chars", len(resp.Text),
				"text_preview", oneLine(resp.Text))
			if violations >= 1 {
				step.Error = "protocol violation: model emitted text without a tool call across retries"
				step.Status = StepFailed
				return false
			}
			violations++
			nudge := llm.Message{Role: llm.RoleUser, Content: executorNudge}
			t.Transcript = append(t.Transcript, nudge)
			t.Session.Append(nudge)
			continue
		}

		finished, done := e.dispatchCalls(t, step, resp.ToolCalls)
		if done {
			return finished
		}
	}

	step.Status = StepFailed
	if step.Error == "" {
		step.Error = fmt.Sprintf("step did not call %s within %d iterations", stepFinishTool, e.maxPerStep)
	}
	return false
}

// dispatchCalls runs every tool_call in the assistant response,
// appending tool-role messages for each. If any call is step.finish,
// the step is marked done with its result and dispatchCalls returns
// (true, true). Otherwise returns (false, false) and the sub-loop
// continues.
func (e *Executor) dispatchCalls(t *Turn, step *Step, calls []llm.ToolCall) (finished bool, done bool) {
	for _, tc := range calls {
		if tc.Function.Name == stepFinishTool {
			var args stepFinishArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				slog.Error("agent: executor: step.finish decode error",
					"step", step.ID, "err", err, "args", tc.Function.Arguments)
				step.Error = fmt.Sprintf("step.finish decode: %v", err)
				step.Status = StepFailed
				return false, true
			}
			tmsg := llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    "ok",
			}
			t.Transcript = append(t.Transcript, tmsg)
			t.Session.Append(tmsg)
			step.Result = strings.TrimSpace(args.Result)
			step.Status = StepDone
			return true, true
		}

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
	return false, false
}

// toolsForStep returns the tool specs advertised to the LLM for this
// step: the planner-listed tools that exist in the registry, plus the
// virtual step.finish tool that terminates the step.
func (e *Executor) toolsForStep(step *Step) ([]llm.ToolSpec, error) {
	all := e.tools.Specs()
	wanted := make(map[string]bool, len(step.Tools))
	for _, t := range step.Tools {
		wanted[t] = true
	}
	out := make([]llm.ToolSpec, 0, len(step.Tools)+1)
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
	out = append(out, llm.ToolSpec{
		Name:        stepFinishTool,
		Description: "Terminate the current step. `result` is the outcome text (one or two sentences) that the synthesizer will consume. Call this exactly once when the step's outcome is known.",
		InputSchema: stepFinishSchema,
	})
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

Every turn must be exactly one tool call. Call a real tool to make
progress; call `+"`step.finish`"+` with the `+"`result`"+` field when the step's
outcome is known. Free-form text without a tool call is a protocol
violation.
</current_step>`, step.ID, step.ID, len(t.Plan.Steps), step.Intent)
	return sys
}
