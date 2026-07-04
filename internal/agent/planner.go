package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
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

// Planner commits a structured plan via plan.commit on an LLM call with
// tool_choice=required. Free-form text is a protocol violation: the
// planner retries once with a stricter nudge and then fails the turn.
// There is no text-wrap fallback — the format is enforced. The tool
// catalogue the model reads to pick step tools lives in
// prompts/system/*.md (D-028); the planner does not synthesise it at
// runtime.
type Planner struct {
	client *llm.Client
	ctxb   *ContextBuilder
	prompt string // contents of prompts/planner.md
}

// plannerNudge is the transcript-appended reminder used after a
// protocol violation. Kept short — the model already saw the full
// planner prompt; this is just the "you broke the contract, do it
// right" note.
const plannerNudge = "PROTOCOL VIOLATION: your previous response was not a plan.commit tool call. You must call plan.commit exactly once. Free-form text is not accepted. Retry."

func NewPlanner(client *llm.Client, ctxb *ContextBuilder, prompt string) *Planner {
	return &Planner{
		client: client,
		ctxb:   ctxb,
		prompt: prompt,
	}
}

// Run executes the planning LLM call and returns a committed Plan. It
// retries once on either a transient LLM call error or a protocol
// violation (no plan.commit tool call), then fails. Both retry-worthy
// failures share the same 2-attempt budget so the caller's terminal
// ❌ reaction fires only when the planner has truly given up.
func (p *Planner) Run(ctx context.Context, env integrations.Envelope, transcript []llm.Message) (*Plan, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("planner: not configured")
	}

	sys := p.ctxb.ComposeSystem(env, p.prompt)
	toolSpec := []llm.ToolSpec{{
		Name:        planCommitTool,
		Description: "Commit an ordered plan for handling the current message. Use exactly once. Each step's intent is the outcome it must produce, not a tool name. Empty tools list = no work, respond-only step.",
		InputSchema: planCommitSchema,
	}}

	msgs := append([]llm.Message{{Role: llm.RoleSystem, Content: sys}}, transcript...)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		logPrompt("agent: planner: prompt", msgs)
		resp, err := p.client.Call(ctx, msgs, toolSpec, llm.ToolChoiceRequired)
		if err != nil {
			slog.Error("agent: planner: LLM ERROR",
				"attempt", attempt, "err", err, "ctx_err", ctx.Err())
			lastErr = err
			if ctx.Err() != nil {
				return nil, fmt.Errorf("planner: llm: %w", err)
			}
			continue
		}
		logResponse("agent: planner: llm response", resp, "attempt", attempt)

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
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: strings.TrimSpace(resp.Text)},
				llm.Message{Role: llm.RoleUser, Content: plannerNudge},
			)
		}
	}

	return nil, fmt.Errorf("planner: exhausted retries: %w", lastErr)
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
