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

// TriageCategory is the routing decision the triage phase commits to.
// Exactly one of the three virtual tools maps to each category.
type TriageCategory string

const (
	TriageCategoryRespond TriageCategory = "respond"
	TriageCategoryPlan    TriageCategory = "plan"
	TriageCategoryStatus  TriageCategory = "status"
)

const (
	triageRespondTool = "triage.respond"
	triagePlanTool    = "triage.plan"
	triageStatusTool  = "triage.status"
)

// TriageResult is the outcome of one triage LLM call. Exactly one
// payload field is populated per category:
//
//   - respond → Reply
//   - plan    → Plan
//   - status  → StatusTool ("status.summary" or "status.report")
//
// Metadata is a forward-compatible bag for future enrichment (intent
// labels, topic detection, etc.). Empty today.
type TriageResult struct {
	Category   TriageCategory
	Reply      string
	Plan       *Plan
	StatusTool string
	Metadata   map[string]any
}

var triageRespondSchema = json.RawMessage(`{
  "type": "object",
  "required": ["reply"],
  "properties": {
    "reply": {"type": "string", "description": "The verbatim message to send to the user. Use ONLY for greetings, thanks, or pure social chit-chat. Never for factual questions, anything that may be in memory, or anything about tobee's current state."}
  }
}`)

var triagePlanSchema = json.RawMessage(`{
  "type": "object",
  "required": ["goal", "steps"],
  "properties": {
    "goal": {"type": "string", "description": "One-sentence statement of the user's goal."},
    "steps": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["intent", "tools"],
        "properties": {
          "intent": {"type": "string", "description": "The result this step must produce. State the outcome, not the procedure."},
          "tools": {
            "type": "array",
            "minItems": 1,
            "items": {"type": "string"},
            "description": "Exact tool names from the <tools> catalogue that the executor is allowed to call on this step. Strictly enforced: anything not listed is unavailable. Include every tool the step might plausibly need."
          },
          "memory_paths": {
            "type": "array",
            "items": {"type": "string"},
            "description": "Specific memory file paths (relative to the memory root) the executor should consult, chosen from the indexes shown in this prompt. Informational hint."
          }
        }
      }
    }
  }
}`)

var triageStatusSchema = json.RawMessage(`{
  "type": "object",
  "required": ["tool"],
  "properties": {
    "tool": {
      "type": "string",
      "enum": ["status.summary", "status.report"],
      "description": "status.summary for general 'what are you up to?' inquiries; status.report when the user wants specifics (failures, schedules, exact next-fire times)."
    }
  }
}`)

// Triage is the pre-processing phase: one LLM call that commits a
// category and its payload via exactly one of three virtual tools. The
// category becomes the state-machine routing decision; the payload
// becomes the inputs for the downstream phase.
type Triage struct {
	client        *llm.Client
	ctxb          *ContextBuilder
	prompt        string // contents of prompts/triage.md
	toolCatalogue string // <tools>…</tools> block; same render as Planner
}

func NewTriage(client *llm.Client, ctxb *ContextBuilder, reg *tools.Registry, prompt string) *Triage {
	return &Triage{
		client:        client,
		ctxb:          ctxb,
		prompt:        prompt,
		toolCatalogue: renderToolCatalogue(reg),
	}
}

// Run executes the triage call and returns the routing decision. The
// transcript is read but not mutated. Errors are fatal to the turn —
// triage failure means we cannot route, so the turn ends with no reply.
func (t *Triage) Run(ctx context.Context, env integrations.Envelope, transcript []llm.Message) (*TriageResult, error) {
	if t == nil || t.client == nil {
		return nil, fmt.Errorf("triage: not configured")
	}

	sys := t.ctxb.ComposeSystem(env, t.prompt, nil)
	if t.toolCatalogue != "" {
		sys += "\n\n" + t.toolCatalogue
	}
	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

	resp, err := t.client.Call(ctx, msgs, []llm.ToolSpec{
		{Name: triageRespondTool, Description: "Reply directly with conversational text. ONLY for greetings, thanks, or social chit-chat. Never for factual questions or anything that may be remembered. If in doubt, do not use this — use triage.plan instead.", InputSchema: triageRespondSchema},
		{Name: triagePlanTool, Description: "Commit a structured plan. Default choice for anything that requires memory lookups, tool actions, or multi-step reasoning. Same shape as the executor's plan contract.", InputSchema: triagePlanSchema},
		{Name: triageStatusTool, Description: "Route to a status tool. Use for questions about what tobee is currently doing, what is scheduled, recent activity, or subsystem state. Picks status.summary or status.report.", InputSchema: triageStatusSchema},
	}, llm.ToolChoiceRequired)
	if err != nil {
		return nil, fmt.Errorf("triage: llm: %w", err)
	}
	slog.Debug("agent: triage: response", "diag", describeResponse(resp))

	return extractTriage(resp)
}

func extractTriage(resp *llm.Response) (*TriageResult, error) {
	for _, tc := range resp.ToolCalls {
		switch tc.Function.Name {
		case triageRespondTool:
			var args struct {
				Reply string `json:"reply"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("triage: decode respond args: %w", err)
			}
			reply := strings.TrimSpace(args.Reply)
			if reply == "" {
				return nil, fmt.Errorf("triage: respond produced empty reply")
			}
			return &TriageResult{Category: TriageCategoryRespond, Reply: reply}, nil

		case triagePlanTool:
			var args commitArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("triage: decode plan args: %w", err)
			}
			plan, err := planFromCommitArgs(args)
			if err != nil {
				return nil, fmt.Errorf("triage: plan: %w", err)
			}
			return &TriageResult{Category: TriageCategoryPlan, Plan: plan}, nil

		case triageStatusTool:
			var args struct {
				Tool string `json:"tool"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				return nil, fmt.Errorf("triage: decode status args: %w", err)
			}
			tool := strings.TrimSpace(args.Tool)
			if tool != "status.summary" && tool != "status.report" {
				return nil, fmt.Errorf("triage: status produced unknown tool %q", tool)
			}
			return &TriageResult{Category: TriageCategoryStatus, StatusTool: tool}, nil
		}
	}
	return nil, fmt.Errorf("triage: produced no recognised tool call (%s)", describeResponse(resp))
}
