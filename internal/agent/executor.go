package agent

import (
	"context"
	"encoding/json"
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

// stepFinishArgs is the JSON shape step_finish emits.
type stepFinishArgs struct {
	Result   string `json:"result"`
	Finished bool   `json:"finished"`
}

// executorNudge is the reminder appended to the transcript after a
// protocol violation inside a step.
const executorNudge = "PROTOCOL VIOLATION: your previous response was neither a tool call nor a step_finish call. You must call exactly one tool per turn. When the step's outcome is known, call step_finish with the result. Free-form text is not accepted. Retry."

// Executor drives one Step at a time through a ReAct sub-loop that
// runs against the shared Conversation. Every tool in the MCP host's
// catalog is advertised on every step's LLM call — D-029 removed per-step
// tool scoping — plus the virtual step_finish tool. Free-form text is a
// protocol violation: retried once, then fails the step.
type Executor struct {
	client      *llm.Client
	tools       *mcphost.Host
	states      *StateTemplates
	maxPerStep  int
	totalBudget int
}

func NewExecutor(client *llm.Client, host *mcphost.Host, states *StateTemplates, maxPerStep, totalBudget int) *Executor {
	if maxPerStep <= 0 {
		maxPerStep = 4
	}
	if totalBudget <= 0 {
		totalBudget = 12
	}
	return &Executor{
		client:      client,
		tools:       host,
		states:      states,
		maxPerStep:  maxPerStep,
		totalBudget: totalBudget,
	}
}

// RunStep appends the rendered execute_step user message to the
// conversation, then runs the ReAct sub-loop against the shared
// Conversation. Mutates step in place (Status, Result, Error,
// Finished, Attempts). Returns true when the step ended cleanly via
// step_finish or a user_ask; the strategy should stop executing when
// the conversation is Finished.
func (e *Executor) RunStep(t *Turn, stepNumber, stepTotal int, step *Step) bool {
	step.Status = StepRunning
	step.Attempts++
	ctx := telemetry.With(t.Ctx, "phase", "execute", "step", step.ID)
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: step begin",
		"num", stepNumber, "of", stepTotal, "intent", step.Intent)

	toolSpecs := e.toolSpecs()
	userMsg, err := e.states.RenderPhase("execute_step", StateData{
		Step:           step,
		StepNumber:     stepNumber,
		StepTotal:      stepTotal,
		Plan:           t.Conversation.Plan,
		AvailableTools: e.toolNames(),
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

		resp, err := callLLM(ctx, e.client, t.Conversation, toolSpecs, llm.ToolChoiceRequired)
		if err != nil {
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
		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		t.Conversation.Append(asst)

		if len(resp.ToolCalls) == 0 {
			logViolation(ctx, violations, stepFinishTool, resp)
			if violations >= 1 {
				step.Error = "protocol violation: model emitted text without a tool call across retries"
				step.Status = StepFailed
				return false
			}
			violations++
			t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: executorNudge})
			continue
		}

		if e.dispatchCalls(ctx, t, step, resp.ToolCalls) {
			return step.Status == StepDone
		}
	}

	step.Status = StepFailed
	if step.Error == "" {
		step.Error = fmt.Sprintf("step did not call %s within %d iterations", stepFinishTool, e.maxPerStep)
	}
	return false
}

// dispatchCalls runs every tool_call in the assistant response,
// appending tool-role messages for each. Returns true when the step is
// now terminal: a step_finish was consumed, or a tool asked the user a
// question and the turn must end (D-036). False means keep iterating.
func (e *Executor) dispatchCalls(ctx context.Context, t *Turn, step *Step, calls []llm.ToolCall) bool {
	for i, tc := range calls {
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
			// The question has been sent; nothing else this turn can use the
			// answer. Close out any calls the model batched after it so the
			// transcript stays a valid tool exchange, then end the turn.
			for _, rest := range calls[i+1:] {
				t.Conversation.Append(llm.Message{
					Role:       llm.RoleTool,
					ToolCallID: rest.ID,
					Name:       rest.Function.Name,
					Content:    "skipped: waiting for the user's answer",
				})
			}
			t.Await = res.Await
			step.Result = "Asked the user: " + res.Await.Question
			step.Status = StepDone
			t.Conversation.Finished = true
			return true
		}
	}
	return false
}

// toolSpecs returns the host's whole catalog plus the virtual step_finish
// tool. Same set advertised on every step's LLM call — D-029 dropped
// per-step scoping.
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

// toolNames returns just the catalog names, for the state template's
// {{.AvailableTools}} field.
func (e *Executor) toolNames() []string {
	return e.tools.ToolNames()
}
