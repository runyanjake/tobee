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
    "result": {"type": "string", "description": "One or two sentences stating the outcome of this step, in the exact form the synthesizer will consume. State the answer, not the procedure."},
    "finished": {"type": "boolean", "description": "Set to true only if the entire user request is now satisfied by the work done so far — the executor will skip any remaining plan steps and jump straight to the synth phase. Set to false (or omit) otherwise."}
  }
}`)

// stepFinishArgs is the JSON shape step.finish emits.
type stepFinishArgs struct {
	Result   string `json:"result"`
	Finished bool   `json:"finished"`
}

// executorNudge is the reminder appended to the transcript after a
// protocol violation inside a step.
const executorNudge = "PROTOCOL VIOLATION: your previous response was neither a tool call nor a step.finish call. You must call exactly one tool per turn. When the step's outcome is known, call step.finish with the result. Free-form text is not accepted. Retry."

// Executor drives one Step at a time through a ReAct sub-loop that
// runs against the shared Conversation. All registered tools are
// advertised on every step's LLM call — D-029 removed per-step tool
// scoping — plus the virtual step.finish tool. Free-form text is a
// protocol violation: retried once, then fails the step.
type Executor struct {
	client      *llm.Client
	tools       *tools.Registry
	states      *StateTemplates
	maxPerStep  int
	totalBudget int
}

func NewExecutor(client *llm.Client, reg *tools.Registry, states *StateTemplates, maxPerStep, totalBudget int) *Executor {
	if maxPerStep <= 0 {
		maxPerStep = 4
	}
	if totalBudget <= 0 {
		totalBudget = 12
	}
	return &Executor{
		client:      client,
		tools:       reg,
		states:      states,
		maxPerStep:  maxPerStep,
		totalBudget: totalBudget,
	}
}

// RunStep appends the rendered execute_step user message to the
// conversation, then runs the ReAct sub-loop against the shared
// Conversation. Mutates step in place (Status, Result, Error,
// Finished, Attempts). Returns true when the step ended cleanly via
// step.finish; the loop should exit early to synth when step.Finished
// is true.
func (e *Executor) RunStep(t *Turn, stepNumber, stepTotal int, step *Step) bool {
	step.Status = StepRunning
	step.Attempts++

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
			slog.Warn("agent: executor: total step budget exhausted",
				"step", step.ID, "steps_run", t.Conversation.Plan.StepsRun, "total_budget", e.totalBudget)
			return false
		}
		t.Conversation.Plan.StepsRun++

		slog.Debug("agent: executor: iteration",
			"step", step.ID, "sub", sub, "steps_run", t.Conversation.Plan.StepsRun,
			"conv_msgs", len(t.Conversation.Messages))

		logPrompt("agent: executor: prompt", t.Conversation.Messages)
		resp, err := e.client.Call(t.Ctx, t.Conversation.Messages, toolSpecs, llm.ToolChoiceRequired)
		if err != nil {
			slog.Error("agent: executor: LLM ERROR",
				"step", step.ID, "sub", sub, "llm_error", llmErrors, "err", err,
				"integration", t.Env.Integration, "channel", t.Env.Channel, "user", t.Env.User,
				"ctx_err", t.Ctx.Err())
			if t.Ctx.Err() != nil {
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
		logResponse("agent: executor: llm response", resp, "step", step.ID, "sub", sub)

		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		t.Conversation.Append(asst)

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
			t.Conversation.Append(llm.Message{Role: llm.RoleUser, Content: executorNudge})
			continue
		}

		if e.dispatchCalls(t, step, resp.ToolCalls) {
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
// appending tool-role messages for each. Returns true when a
// step.finish call was consumed (step is now terminal); false when
// more iterations are needed.
func (e *Executor) dispatchCalls(t *Turn, step *Step, calls []llm.ToolCall) bool {
	for _, tc := range calls {
		if tc.Function.Name == stepFinishTool {
			var args stepFinishArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				slog.Error("agent: executor: step.finish decode error",
					"step", step.ID, "err", err, "args", tc.Function.Arguments)
				step.Error = fmt.Sprintf("step.finish decode: %v", err)
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
			if args.Finished {
				t.Conversation.Finished = true
			}
			return true
		}

		out, cerr := e.tools.Call(t.Ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
		content := out
		if cerr != nil {
			slog.Warn("agent: tool error", "tool", tc.Function.Name, "err", cerr)
			content = fmt.Sprintf("error: %v", cerr)
		} else {
			slog.Debug("agent: tool ok", "tool", tc.Function.Name)
		}
		t.Conversation.Append(llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    content,
		})
	}
	return false
}

// toolSpecs returns every registered tool plus the virtual step.finish
// tool. Same set advertised on every step's LLM call — D-029 dropped
// per-step scoping.
func (e *Executor) toolSpecs() []llm.ToolSpec {
	all := e.tools.Specs()
	out := make([]llm.ToolSpec, 0, len(all)+1)
	out = append(out, all...)
	out = append(out, llm.ToolSpec{
		Name:        stepFinishTool,
		Description: "Terminate the current step. `result` is the outcome text (one or two sentences) the synthesizer will consume. Set `finished: true` only if the entire user request is now satisfied — the executor will skip any remaining plan steps and jump to synth.",
		InputSchema: stepFinishSchema,
	})
	return out
}

// toolNames returns just the names of the registered tools, for the
// state template's {{.AvailableTools}} field.
func (e *Executor) toolNames() []string {
	specs := e.tools.Specs()
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}
