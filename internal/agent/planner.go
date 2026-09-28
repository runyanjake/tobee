package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/telemetry"
)

const planCommitTool = "plan_commit"

// planCommitSchema: steps carry no tool scoping (D-029); empty steps plus direct_reply is the fast path (D-032).
var planCommitSchema = json.RawMessage(`{
  "type": "object",
  "required": ["goal", "steps"],
  "properties": {
    "goal": {"type": "string", "description": "One-sentence statement of the user's goal."},
    "direct_reply": {"type": "string", "description": "The complete answer to the user, when answering needs no tools and no steps. Set this and leave steps empty."},
    "steps": {
      "type": "array",
      "minItems": 0,
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

// Planner offers only plan_commit; invalid output is retried once, then fails the turn (D-025, D-041).
type Planner struct {
	model  llm.Model
	states *StateTemplates
}

const plannerNudge = "Your last response could not be read as a plan_commit call. Call plan_commit."

func NewPlanner(model llm.Model, states *StateTemplates) *Planner {
	return &Planner{model: model, states: states}
}

// Run sets conv.Plan, retrying once on an LLM error or protocol violation.
func (p *Planner) Run(ctx context.Context, conv *Conversation, request []llm.Message) error {
	if p == nil || p.model == nil {
		return fmt.Errorf("planner: not configured")
	}
	ctx = telemetry.With(ctx, "phase", "plan")

	// Never fuse user text with the phase directive; the model must tell them apart (D-029).
	for _, m := range request {
		conv.Append(m)
	}

	phaseMsg, err := p.states.RenderPhase("plan", StateData{})
	if err != nil {
		return fmt.Errorf("planner: render plan state: %w", err)
	}
	conv.Append(llm.Message{Role: llm.RoleUser, Content: phaseMsg})

	toolSpec := []llm.ToolSpec{{
		Name:        planCommitTool,
		Description: "Commit an ordered plan for handling the current message. Use exactly once. Each step's intent is the outcome it must produce, not a tool name.",
		InputSchema: planCommitSchema,
	}}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		d, err := decide(ctx, p.model, conv, toolSpec)
		if errors.Is(err, llm.ErrInvalidDecision) {
			// Unreadable output is dropped; keeping it teaches the next call to repeat it.
			lastErr = err
			if attempt == 0 {
				conv.Append(llm.Message{Role: llm.RoleUser, Content: plannerNudge})
			}
			continue
		}
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return fmt.Errorf("planner: llm: %w", err)
			}
			continue
		}

		tc := d.Call
		conv.Append(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}})
		var args commitArgs
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Errorf("planner: decode %s args: %w", planCommitTool, err)
		}
		plan, perr := planFromCommitArgs(args)
		if perr != nil {
			return fmt.Errorf("planner: %w", perr)
		}
		// Ack the call so the next request has no dangling assistant tool call.
		conv.Append(llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    "ok",
		})
		conv.Plan = plan
		logPlan(ctx, plan)
		return nil
	}

	return fmt.Errorf("planner: exhausted retries: %w", lastErr)
}

func logPlan(ctx context.Context, plan *Plan) {
	if plan.DirectReply != "" {
		telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: plan",
			"goal", plan.Goal, "route", "direct_reply")
		return
	}
	intents := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		intents[i] = fmt.Sprintf("%s: %s", s.ID, s.Intent)
	}
	telemetry.Log(ctx, slog.LevelInfo, telemetry.Thinking, "agent: plan",
		"goal", plan.Goal, "route", "steps", "steps", intents)
}

type commitArgs struct {
	Goal        string `json:"goal"`
	DirectReply string `json:"direct_reply"`
	Steps       []struct {
		Intent string `json:"intent"`
	} `json:"steps"`
}

// planFromCommitArgs: zero steps is legal only with a direct_reply (D-032).
func planFromCommitArgs(args commitArgs) (*Plan, error) {
	plan := &Plan{
		Goal:        strings.TrimSpace(args.Goal),
		DirectReply: strings.TrimSpace(args.DirectReply),
	}
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
		if plan.DirectReply == "" {
			return nil, fmt.Errorf("zero usable steps and no direct_reply")
		}
		return plan, nil
	}
	// A plan that needs execution must not ship a reply written before it ran.
	plan.DirectReply = ""
	plan.assignIDs()
	return plan, nil
}
