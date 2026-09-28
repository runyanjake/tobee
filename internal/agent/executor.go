package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/telemetry"
)

const stepFinishTool = "step_finish"

var stepFinishSchema = json.RawMessage(`{
  "type": "object",
  "required": ["result"],
  "properties": {
    "result": {"type": "string", "description": "One or two sentences stating the outcome of this step, in the exact form the synthesizer will consume. State the answer, not the procedure."},
    "finished": {"type": "boolean", "description": "Set to true only if the entire user request is now satisfied by the work done so far — the executor will skip any remaining plan steps and jump straight to the synth phase. Set to false (or omit) otherwise."}
  }
}`)

type stepFinishArgs struct {
	Result   string `json:"result"`
	Finished bool   `json:"finished"`
}

// executorNudge follows output that was not a tool call; one retry is allowed (D-025).
const executorNudge = "Your last response could not be read as a tool call. Call exactly one tool. When the step's outcome is known, call step_finish."

// Executor runs one Step's ReAct sub-loop with the full catalog plus step_finish (D-029, D-041).
type Executor struct {
	model       llm.Model
	tools       *mcphost.Host
	states      *StateTemplates
	maxPerStep  int
	totalBudget int
}

func NewExecutor(model llm.Model, host *mcphost.Host, states *StateTemplates, maxPerStep, totalBudget int) *Executor {
	if maxPerStep <= 0 {
		maxPerStep = 4
	}
	if totalBudget <= 0 {
		totalBudget = 12
	}
	return &Executor{
		model:       model,
		tools:       host,
		states:      states,
		maxPerStep:  maxPerStep,
		totalBudget: totalBudget,
	}
}

// RunStep mutates step in place and reports whether it ended via step_finish or user_ask.
func (e *Executor) RunStep(t *Turn, stepNumber, stepTotal int, step *Step) bool {
	step.Status = StepRunning
	step.Attempts++
	ctx := telemetry.With(t.Ctx, "phase", "execute", "step", step.ID)
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: step begin",
		"num", stepNumber, "of", stepTotal, "intent", step.Intent)

	toolSpecs := e.toolSpecs()
	userMsg, err := e.states.RenderPhase("execute_step", StateData{
		Step:       step,
		StepNumber: stepNumber,
		StepTotal:  stepTotal,
		Plan:       t.Conversation.Plan,
	})
	if err != nil {
		step.Error = fmt.Sprintf("render execute_step: %v", err)
		step.Status = StepFailed
		return false
	}
	t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: userMsg})

	violations := 0
	llmErrors := 0
	for sub := 0; sub < e.maxPerStep; sub++ {
		if t.Conversation.Plan.StepsRun >= e.totalBudget {
			step.Error = "turn step-budget exhausted"
			step.Status = StepFailed
			telemetry.Logger(ctx).Warn("agent: executor: total step budget exhausted",
				"steps_run", t.Conversation.Plan.StepsRun, "total_budget", e.totalBudget)
			return false
		}
		t.Conversation.Plan.StepsRun++

		d, err := decide(ctx, e.model, t.Conversation, toolSpecs)
		switch {
		case errors.Is(err, llm.ErrInvalidDecision):
			if violations >= 1 {
				step.Error = "model output was not a valid tool call across retries"
				step.Status = StepFailed
				return false
			}
			violations++
			t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: executorNudge})
			continue
		case err != nil:
			if ctx.Err() != nil {
				step.Error = err.Error()
				step.Status = StepFailed
				return false
			}
			if llmErrors >= 1 {
				step.Error = fmt.Sprintf("llm error across retries: %v", err)
				step.Status = StepFailed
				return false
			}
			llmErrors++
			continue
		}
		t.Conversation.Append(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{d.Call}})

		if e.dispatch(ctx, t, step, d.Call) {
			return step.Status == StepDone
		}
	}

	step.Status = StepFailed
	if step.Error == "" {
		step.Error = fmt.Sprintf("step did not call %s within %d iterations", stepFinishTool, e.maxPerStep)
	}
	return false
}

// dispatch runs one tool call; true means the step is terminal (step_finish or a user question, D-036).
func (e *Executor) dispatch(ctx context.Context, t *Turn, step *Step, tc llm.ToolCall) bool {
	if tc.Function.Name == stepFinishTool {
		var args stepFinishArgs
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			telemetry.Logger(ctx).Error("agent: executor: step_finish decode error",
				"err", err, "args", tc.Function.Arguments)
			step.Error = fmt.Sprintf("step_finish decode: %v", err)
			step.Status = StepFailed
			return true
		}
		t.Conversation.Append(llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    "ok",
		})
		step.Result = strings.TrimSpace(args.Result)
		step.Finished = args.Finished
		step.Status = StepDone
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: step result",
			telemetry.Content("result", step.Result), "finished", args.Finished)
		if args.Finished {
			t.Conversation.Finished = true
		}
		return true
	}

	telemetry.Log(ctx, slog.LevelInfo, telemetry.Action, "agent: tool call",
		"tool", tc.Function.Name, "call_id", tc.ID, telemetry.Content("args", tc.Function.Arguments))
	start := time.Now()
	res, cerr := e.tools.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	content := res.Text
	status, level := "ok", slog.LevelInfo
	switch {
	case cerr != nil:
		content = fmt.Sprintf("error: %v", cerr)
		status, level = "failed", slog.LevelWarn
	case res.IsError:
		content = "error: " + res.Text
		status, level = "error", slog.LevelWarn
	case res.Await != nil:
		status = "await"
	case res.Verbatim:
		t.AddVerbatim(tc.Function.Name, res.Text)
		status = "verbatim"
	}
	telemetry.Log(ctx, level, telemetry.Action, "agent: tool result",
		"tool", tc.Function.Name, "call_id", tc.ID, "status", status,
		"duration_ms", time.Since(start).Milliseconds(), telemetry.Content("content", content))
	t.Conversation.Append(llm.Message{
		Role:       llm.RoleTool,
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
		Content:    content,
	})

	if cerr == nil && res.Await != nil {
		// Nothing else this turn can use the answer, so end the turn.
		t.Await = res.Await
		step.Result = "Asked the user: " + res.Await.Question
		step.Status = StepDone
		t.Conversation.Finished = true
		return true
	}
	return false
}

// toolSpecs is the same set on every call: no per-step scoping (D-029).
func (e *Executor) toolSpecs() []llm.ToolSpec {
	all := e.tools.Tools()
	out := make([]llm.ToolSpec, 0, len(all)+1)
	out = append(out, all...)
	out = append(out, llm.ToolSpec{
		Name:        stepFinishTool,
		Description: "Terminate the current step. `result` is the outcome text (one or two sentences) the synthesizer will consume. Set `finished: true` only if the entire user request is now satisfied — the executor will skip any remaining plan steps and jump to synth.",
		InputSchema: stepFinishSchema,
	})
	return out
}
