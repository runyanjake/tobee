package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

const planCommitTool = "plan.commit"

// planCommitSchema is the structured plan shape the planner LLM emits
// via the plan.commit virtual tool. Simplified under D-029: steps no
// longer carry `tools` or `memory_paths` — every step has every
// registered tool available. Status, IDs, and run counts are owned by
// the loop, not the model.
var planCommitSchema = json.RawMessage(`{
  "type": "object",
  "required": ["goal", "steps"],
  "properties": {
    "goal": {"type": "string", "description": "One-sentence statement of the user's goal."},
    "steps": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["intent"],
        "properties": {
          "intent": {"type": "string", "description": "The outcome this step must produce. State the result, not the procedure."}
        }
      }
    }
  }
}`)

// Planner commits a structured plan via plan.commit on an LLM call
// with tool_choice=required. Free-form text is a protocol violation:
// the planner retries once with a stricter nudge and then fails the
// turn. There is no text-wrap fallback — the format is enforced.
type Planner struct {
	client *llm.Client
	states *StateTemplates
}

// plannerNudge is the transcript-appended reminder used after a
// protocol violation. Kept short — the LLM already read the plan
// state template; this is just the "you broke the contract, do it
// right" note.
const plannerNudge = "PROTOCOL VIOLATION: your previous response was not a plan.commit tool call. You must call plan.commit exactly once. Free-form text is not accepted. Retry."

func NewPlanner(client *llm.Client, states *StateTemplates) *Planner {
	return &Planner{client: client, states: states}
}

// Run appends the rendered plan-state message to the conversation,
// makes the planning LLM call, and appends the resulting assistant
// message. On success, conv.Plan is set. Retries once on either a
// transient LLM call error or a protocol violation.
func (p *Planner) Run(ctx context.Context, conv *Conversation, userInput string) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("planner: not configured")
	}

	userMsg, err := p.states.Render("plan", StateData{UserInput: userInput})
	if err != nil {
		return fmt.Errorf("planner: render plan state: %w", err)
	}
	conv.Append(llm.Message{Role: llm.RoleUser, Content: userMsg})

	toolSpec := []llm.ToolSpec{{
		Name:        planCommitTool,
		Description: "Commit an ordered plan for handling the current message. Use exactly once. Each step's intent is the outcome it must produce, not a tool name.",
		InputSchema: planCommitSchema,
	}}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		logPrompt("agent: planner: prompt", conv.Messages)
		resp, err := p.client.Call(ctx, conv.Messages, toolSpec, llm.ToolChoiceRequired)
		if err != nil {
			slog.Error("agent: planner: LLM ERROR",
				"attempt", attempt, "err", err, "ctx_err", ctx.Err())
			lastErr = err
			if ctx.Err() != nil {
				return fmt.Errorf("planner: llm: %w", err)
			}
			continue
		}
		logResponse("agent: planner: llm response", resp, "attempt", attempt)

		asst := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		}
		conv.Append(asst)

		for _, tc := range resp.ToolCalls {
			if tc.Function.Name != planCommitTool {
				continue
			}
			var args commitArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return fmt.Errorf("planner: decode %s args: %w", planCommitTool, err)
			}
			plan, perr := planFromCommitArgs(args)
			if perr != nil {
				return fmt.Errorf("planner: %w", perr)
			}
			// Ack the tool call in the conversation so the next
			// LLM turn sees a valid tool exchange rather than a
			// dangling assistant tool_call.
			conv.Append(llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    "ok",
			})
			conv.Plan = plan
			return nil
		}

		slog.Error("agent: planner: PROTOCOL VIOLATION",
			"attempt", attempt,
			"expected_tool", planCommitTool,
			"finish", resp.Finish,
			"text_chars", len(resp.Text),
			"text_preview", oneLine(resp.Text),
			"tool_calls_count", len(resp.ToolCalls),
			"tool_calls", renderToolCalls(resp.ToolCalls))
		lastErr = fmt.Errorf("protocol violation: no %s call", planCommitTool)

		if attempt == 0 {
			conv.Append(llm.Message{Role: llm.RoleUser, Content: plannerNudge})
		}
	}

	return fmt.Errorf("planner: exhausted retries: %w", lastErr)
}

// commitArgs is the JSON shape plan.commit emits.
type commitArgs struct {
	Goal  string `json:"goal"`
	Steps []struct {
		Intent string `json:"intent"`
	} `json:"steps"`
}

// planFromCommitArgs builds a *Plan from the decoded JSON, trimming
// intents and skipping empty steps. Returns an error if no usable
// steps remain.
func planFromCommitArgs(args commitArgs) (*Plan, error) {
	plan := &Plan{Goal: strings.TrimSpace(args.Goal)}
	for _, s := range args.Steps {
		intent := strings.TrimSpace(s.Intent)
		if intent == "" {
			continue
		}
		plan.Steps = append(plan.Steps, Step{
			Intent: intent,
			Status: StepPending,
		})
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("zero usable steps")
	}
	plan.assignIDs()
	return plan, nil
}
