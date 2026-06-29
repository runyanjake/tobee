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

const planReviseTool = "plan.revise"

// planReviseSchema is advertised to the planner LLM on Revise calls.
// Identical shape to the triage.plan schema; structural duplication is
// fine because the planner persona is different and reasoning lives in
// the prompt, not the schema.
var planReviseSchema = json.RawMessage(`{
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
            "description": "Exact tool names from the <tools> catalogue that the executor is allowed to call on this step. Strictly enforced."
          },
          "memory_paths": {
            "type": "array",
            "items": {"type": "string"},
            "description": "Specific memory file paths the executor should consult."
          }
        }
      }
    }
  }
}`)

// Planner produces revised plans after a step fails. Initial plan
// commitment lives on the Triage type — the planner only runs when
// closed-loop replan is needed. The replan call sees a planner-specific
// persona and the same <tools> catalogue Triage uses.
type Planner struct {
	client        *llm.Client
	ctxb          *ContextBuilder
	prompt        string // contents of prompts/planner.md
	toolCatalogue string
}

func NewPlanner(client *llm.Client, ctxb *ContextBuilder, reg *tools.Registry, prompt string) *Planner {
	return &Planner{
		client:        client,
		ctxb:          ctxb,
		prompt:        prompt,
		toolCatalogue: renderToolCatalogue(reg),
	}
}

// Revise reruns the planner after a step failed. The prior plan and the
// failure reason are passed in a <replan> system reminder. Returns a
// fresh Plan whose Replans counter has been advanced.
func (p *Planner) Revise(ctx context.Context, env integrations.Envelope, prev *Plan, reason string, transcript []llm.Message) (*Plan, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("planner: not configured")
	}

	sys := p.ctxb.ComposeSystem(env, p.prompt, prev)
	if p.toolCatalogue != "" {
		sys += "\n\n" + p.toolCatalogue
	}
	sys += fmt.Sprintf("\n\n<replan>\nPrior plan did not complete. Revise.\nReason: %s\n</replan>",
		strings.TrimSpace(reason))

	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

	resp, err := p.client.Call(ctx, msgs, []llm.ToolSpec{{
		Name:        planReviseTool,
		Description: "Commit a revised plan after a prior step failed.",
		InputSchema: planReviseSchema,
	}})
	if err != nil {
		return nil, fmt.Errorf("planner: revise llm: %w", err)
	}
	slog.Debug("agent: planner: response", "kind", "revise", "diag", describeResponse(resp))

	for _, tc := range resp.ToolCalls {
		if tc.Function.Name != planReviseTool {
			continue
		}
		var args commitArgs
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("planner: decode %s args: %w", planReviseTool, err)
		}
		plan, err := planFromCommitArgs(args)
		if err != nil {
			return nil, fmt.Errorf("planner: revise: %w", err)
		}
		plan.Replans = prev.Replans + 1
		return plan, nil
	}
	return nil, fmt.Errorf("planner: revise produced no plan (%s)", describeResponse(resp))
}

// commitArgs is the JSON shape both triage.plan and plan.revise emit.
type commitArgs struct {
	Goal  string `json:"goal"`
	Steps []struct {
		Intent      string   `json:"intent"`
		Tools       []string `json:"tools"`
		MemoryPaths []string `json:"memory_paths"`
	} `json:"steps"`
}

// planFromCommitArgs builds a *Plan from the decoded JSON, trimming
// strings and skipping empty steps. Returns an error if no usable steps
// remain.
func planFromCommitArgs(args commitArgs) (*Plan, error) {
	plan := &Plan{Goal: strings.TrimSpace(args.Goal)}
	for _, s := range args.Steps {
		intent := strings.TrimSpace(s.Intent)
		if intent == "" {
			continue
		}
		plan.Steps = append(plan.Steps, Step{
			Intent:      intent,
			Tools:       trimStrings(s.Tools),
			MemoryPaths: trimStrings(s.MemoryPaths),
			Status:      StepPending,
		})
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("zero usable steps")
	}
	plan.assignIDs()
	return plan, nil
}

// renderToolCatalogue produces the <tools> block injected into the
// triage and planner system prompts. Lists executor tool names + one-line
// descriptions so the routing/replan model can pick which tools each
// step will need. Returns "" if the registry is nil or empty.
func renderToolCatalogue(reg *tools.Registry) string {
	if reg == nil {
		return ""
	}
	specs := reg.Specs()
	if len(specs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<tools>\n")
	sb.WriteString("These are the tools the executor can call when working a step. Plan steps that use them when they fit.\n")
	for _, s := range specs {
		desc := oneLine(s.Description)
		if desc == "" {
			fmt.Fprintf(&sb, "- %s\n", s.Name)
		} else {
			fmt.Fprintf(&sb, "- %s — %s\n", s.Name, desc)
		}
	}
	sb.WriteString("</tools>")
	return sb.String()
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

func trimStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// describeResponse renders a short diagnostic of an LLM response for
// use inside error messages.
func describeResponse(resp *llm.Response) string {
	if resp == nil {
		return "nil response"
	}
	parts := []string{
		fmt.Sprintf("finish=%q", resp.Finish),
		fmt.Sprintf("text_chars=%d", len(resp.Text)),
		fmt.Sprintf("tool_calls=%d", len(resp.ToolCalls)),
	}
	if len(resp.ToolCalls) > 0 {
		names := make([]string, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		parts = append(parts, fmt.Sprintf("tools=[%s]", strings.Join(names, ",")))
	}
	return strings.Join(parts, " ")
}
