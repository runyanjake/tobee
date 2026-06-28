package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StepStatus tracks one Step's progress through the turn's execution.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// Step is one unit of work the planner committed to. Intent is what the
// executor must achieve, stated as a result. Tools to use are the executor's
// call, not the planner's.
type Step struct {
	ID       string     `json:"id"`
	Intent   string     `json:"intent"`
	Status   StepStatus `json:"status"`
	Result   string     `json:"result,omitempty"`
	Error    string     `json:"error,omitempty"`
	Attempts int        `json:"attempts,omitempty"`
}

// Plan is the turn-scoped artifact the planner produces. It is rendered
// into the system prompt at every executor iteration so the model can see
// its own state. Linear; no DAG.
type Plan struct {
	Goal     string `json:"goal"`
	Steps    []Step `json:"steps"`
	Replans  int    `json:"replans,omitempty"`
	StepsRun int    `json:"-"` // total executor LLM iterations across all steps
}

// Next returns a pointer to the first pending or running step, or nil if
// none remain. Mutations through the returned pointer write through to p.
func (p *Plan) Next() *Step {
	if p == nil {
		return nil
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Status == StepPending || s.Status == StepRunning {
			return s
		}
	}
	return nil
}

// Complete reports whether every step has reached a terminal state.
func (p *Plan) Complete() bool {
	if p == nil {
		return true
	}
	for _, s := range p.Steps {
		if s.Status == StepPending || s.Status == StepRunning {
			return false
		}
	}
	return true
}

// HasMultipleSteps reports whether the plan committed more than one step.
// Used to decide whether a synthesis call is warranted.
func (p *Plan) HasMultipleSteps() bool {
	return p != nil && len(p.Steps) > 1
}

// LastStepText returns the most recent step's Result, used as a fallback
// reply when the plan was single-step (no synthesis call).
func (p *Plan) LastStepText() string {
	if p == nil {
		return ""
	}
	for i := len(p.Steps) - 1; i >= 0; i-- {
		if p.Steps[i].Result != "" {
			return p.Steps[i].Result
		}
	}
	return ""
}

// RenderUserMessage returns the plan-announcement text sent to the user
// immediately after a multi-step plan is committed. Single-step plans
// skip this; the final reply lands directly.
func (p *Plan) RenderUserMessage() string {
	if p == nil || len(p.Steps) == 0 {
		return ""
	}
	var sb strings.Builder
	if goal := strings.TrimSpace(p.Goal); goal != "" {
		fmt.Fprintf(&sb, "Working on: %s\n\n", goal)
	} else {
		sb.WriteString("Working on it.\n\n")
	}
	for i, s := range p.Steps {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.TrimSpace(s.Intent))
	}
	sb.WriteString("\nI'll reply when done.")
	return sb.String()
}

// RenderStepStatus produces a one-line internal marker appended to the
// session after a step terminates. Not sent to the user — it lets the
// next turn's planner (and a future replan-from-thought loop) read step
// progress out of the recent ring even after the in-flight plan is gone.
func (p *Plan) RenderStepStatus(step *Step) string {
	if p == nil || step == nil {
		return ""
	}
	idx := 0
	for i := range p.Steps {
		if p.Steps[i].ID == step.ID {
			idx = i + 1
			break
		}
	}
	intent := truncateOneLine(step.Intent, 60)
	switch step.Status {
	case StepDone:
		return fmt.Sprintf("[progress] step %d/%d (%s): done", idx, len(p.Steps), intent)
	case StepFailed:
		reason := truncateOneLine(step.Error, 80)
		if reason == "" {
			reason = "unknown error"
		}
		return fmt.Sprintf("[progress] step %d/%d (%s): failed - %s", idx, len(p.Steps), intent, reason)
	default:
		return ""
	}
}

func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		s = s[:max-3] + "..."
	}
	return s
}

// Render returns the <plan> block injected into the system prompt.
// Empty when the plan has no steps yet.
func (p *Plan) Render() string {
	if p == nil || len(p.Steps) == 0 {
		return ""
	}
	body, err := json.MarshalIndent(struct {
		Goal  string `json:"goal"`
		Steps []Step `json:"steps"`
	}{Goal: p.Goal, Steps: p.Steps}, "", "  ")
	if err != nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<plan>\n")
	sb.Write(body)
	sb.WriteString("\n</plan>")
	return sb.String()
}

// assignIDs gives every step a stable ID (s1, s2, …) and a default
// status. Idempotent — existing IDs and statuses are preserved.
func (p *Plan) assignIDs() {
	for i := range p.Steps {
		if p.Steps[i].ID == "" {
			p.Steps[i].ID = fmt.Sprintf("s%d", i+1)
		}
		if p.Steps[i].Status == "" {
			p.Steps[i].Status = StepPending
		}
	}
}
