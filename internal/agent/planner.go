package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
)

const (
	planCommitTool = "plan.commit"
	planReviseTool = "plan.revise"
)

// planCommitSchema is advertised to the planner LLM on Initial and Revise
// calls. Status, IDs and replan counts are owned by the loop, not the model.
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
          "intent": {"type": "string", "description": "What this step must achieve. The outcome, not the tool."}
        }
      }
    }
  }
}`)

// Planner produces and revises a turn's Plan. It is a thin wrapper around
// the LLM client with a planner-specific system prompt and a single
// virtual tool (plan.commit / plan.revise) whose output is decoded here
// rather than dispatched through the global tool registry.
type Planner struct {
	client *llm.Client
	ctxb   *ContextBuilder
	prompt string // contents of prompts/planner.md
}

func NewPlanner(client *llm.Client, ctxb *ContextBuilder, prompt string) *Planner {
	return &Planner{client: client, ctxb: ctxb, prompt: prompt}
}

// commitArgs is the JSON shape the planner emits via plan.commit/plan.revise.
type commitArgs struct {
	Goal  string `json:"goal"`
	Steps []struct {
		Intent string `json:"intent"`
	} `json:"steps"`
}

// Initial runs the first planning call of a turn. Returns either a
// committed plan or trivial-reply text. Exactly one of the two will be
// non-nil / non-empty in the happy path; both empty is an error.
func (p *Planner) Initial(ctx context.Context, env integrations.Envelope, transcript []llm.Message) (*Plan, string, error) {
	if p == nil || p.client == nil {
		return nil, "", fmt.Errorf("planner: not configured")
	}

	sys := p.ctxb.ComposeSystem(env, p.prompt, nil)
	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

	resp, err := p.client.Call(ctx, msgs, []llm.ToolSpec{{
		Name:        planCommitTool,
		Description: "Commit an ordered plan for handling the current message. Use exactly once. Each step's intent is a result, not a tool name.",
		InputSchema: planCommitSchema,
	}})
	if err != nil {
		return nil, "", fmt.Errorf("planner: llm: %w", err)
	}

	plan, text, err := extractPlan(resp, planCommitTool)
	if err != nil {
		return nil, "", err
	}
	if plan != nil {
		return plan, "", nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", fmt.Errorf("planner: produced neither plan nor reply")
	}
	return nil, text, nil
}

// Revise reruns the planner after a step failed. The prior plan and the
// failure reason are passed in a <replan> system reminder so the model
// has the context. Returns a fresh Plan whose Replans counter has been
// advanced; StepsRun and prior step status are not preserved (the
// executor restarts from the new plan's first step).
func (p *Planner) Revise(ctx context.Context, env integrations.Envelope, prev *Plan, reason string, transcript []llm.Message) (*Plan, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("planner: not configured")
	}

	sys := p.ctxb.ComposeSystem(env, p.prompt, prev)
	sys += fmt.Sprintf("\n\n<replan>\nPrior plan did not complete. Revise.\nReason: %s\n</replan>",
		strings.TrimSpace(reason))

	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

	resp, err := p.client.Call(ctx, msgs, []llm.ToolSpec{{
		Name:        planReviseTool,
		Description: "Commit a revised plan after a prior step failed. Same shape as plan.commit.",
		InputSchema: planCommitSchema,
	}})
	if err != nil {
		return nil, fmt.Errorf("planner: revise llm: %w", err)
	}
	plan, _, err := extractPlan(resp, planReviseTool)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, fmt.Errorf("planner: revise produced no plan")
	}
	plan.Replans = prev.Replans + 1
	return plan, nil
}

// extractPlan picks the named virtual tool call out of resp and decodes
// its arguments into a Plan. Returns (nil, text, nil) when the tool was
// not called — caller can use text as a trivial reply.
func extractPlan(resp *llm.Response, toolName string) (*Plan, string, error) {
	for _, tc := range resp.ToolCalls {
		if tc.Function.Name != toolName {
			continue
		}
		var args commitArgs
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return nil, "", fmt.Errorf("planner: decode %s args: %w", toolName, err)
		}
		plan := &Plan{Goal: strings.TrimSpace(args.Goal)}
		for _, s := range args.Steps {
			intent := strings.TrimSpace(s.Intent)
			if intent == "" {
				continue
			}
			plan.Steps = append(plan.Steps, Step{Intent: intent, Status: StepPending})
		}
		if len(plan.Steps) == 0 {
			return nil, "", fmt.Errorf("planner: %s produced zero steps", toolName)
		}
		plan.assignIDs()
		return plan, resp.Text, nil
	}
	return nil, resp.Text, nil
}
