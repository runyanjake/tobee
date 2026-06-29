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

const planCommitTool = "plan.commit"

// planCommitSchema is the structured plan shape the planner LLM emits
// via the plan.commit virtual tool. Status, IDs, and run counts are
// owned by the loop, not the model.
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
          "intent": {"type": "string", "description": "The outcome this step must produce. State the result, not the procedure."},
          "tools": {
            "type": "array",
            "items": {"type": "string"},
            "description": "Exact tool names from the <tools> catalogue available to the executor on this step. Strictly enforced. Empty list = no work to do; the executor recognises this as a respond-only step."
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

// Planner commits a structured plan via plan.commit on a single LLM
// call with tool_choice=required. If the model emits text instead
// (model non-compliance with the tool protocol), the text is wrapped
// as a one-step Plan whose Result is pre-populated, letting the rest
// of the loop proceed uniformly — see Planner.Run.
type Planner struct {
	client        *llm.Client
	ctxb          *ContextBuilder
	prompt        string // contents of prompts/planner.md
	toolCatalogue string // <tools>…</tools> block; same render as Executor sees
}

func NewPlanner(client *llm.Client, ctxb *ContextBuilder, reg *tools.Registry, prompt string) *Planner {
	return &Planner{
		client:        client,
		ctxb:          ctxb,
		prompt:        prompt,
		toolCatalogue: renderToolCatalogue(reg),
	}
}

// Run executes the planning LLM call and returns a Plan. Plan is
// always non-nil on a nil error — the text-wrap fallback ensures
// trivial-input non-compliance still produces a usable plan.
func (p *Planner) Run(ctx context.Context, env integrations.Envelope, transcript []llm.Message) (*Plan, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("planner: not configured")
	}

	sys := p.ctxb.ComposeSystem(env, p.prompt)
	if p.toolCatalogue != "" {
		sys += "\n\n" + p.toolCatalogue
	}
	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

	logPrompt("agent: planner: prompt", msgs)
	resp, err := p.client.Call(ctx, msgs, []llm.ToolSpec{{
		Name:        planCommitTool,
		Description: "Commit an ordered plan for handling the current message. Use exactly once. Each step's intent is the outcome it must produce, not a tool name. Empty tools list = no work, respond-only step.",
		InputSchema: planCommitSchema,
	}}, llm.ToolChoiceRequired)
	if err != nil {
		return nil, fmt.Errorf("planner: llm: %w", err)
	}
	slog.Debug("agent: planner: llm response",
		"finish", resp.Finish,
		"text_chars", len(resp.Text),
		"text", resp.Text,
		"tool_calls_count", len(resp.ToolCalls),
		"tool_calls", renderToolCalls(resp.ToolCalls))

	// Happy path: the model called plan.commit.
	for _, tc := range resp.ToolCalls {
		if tc.Function.Name != planCommitTool {
			continue
		}
		var args commitArgs
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("planner: decode %s args: %w", planCommitTool, err)
		}
		plan, perr := planFromCommitArgs(args)
		if perr != nil {
			return nil, fmt.Errorf("planner: %w", perr)
		}
		return plan, nil
	}

	// Fallback: the model emitted text instead of the tool call. Wrap
	// it as a single-step plan whose Result is the text so the rest of
	// the loop runs uniformly. The synthesizer turns this into the
	// final user-facing reply on the trivial path.
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return nil, fmt.Errorf("planner: produced neither plan nor text")
	}
	slog.Warn("agent: planner: model emitted text instead of plan.commit; wrapping",
		"text_chars", len(text))
	return &Plan{
		Goal: "respond to the user",
		Steps: []Step{{
			ID:     "s1",
			Intent: "respond to the user",
			Status: StepDone,
			Result: text,
		}},
	}, nil
}

// commitArgs is the JSON shape plan.commit emits.
type commitArgs struct {
	Goal  string `json:"goal"`
	Steps []struct {
		Intent      string   `json:"intent"`
		Tools       []string `json:"tools"`
		MemoryPaths []string `json:"memory_paths"`
	} `json:"steps"`
}

// planFromCommitArgs builds a *Plan from the decoded JSON, trimming
// strings and skipping empty steps. Returns an error if no usable
// steps remain after trimming.
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
// planner's system prompt. Lists executor-callable tools by name +
// one-line description so the planner can plan steps that use them.
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
	sb.WriteString("These are the tools the executor can call when working a step. List exactly the tools each step might plausibly need.\n")
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
