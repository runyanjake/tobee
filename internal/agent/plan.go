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
	StepSkipped StepStatus = "skipped" // set when an earlier step's finished=true short-circuits
)

// Step is one unit of work the planner committed to. Intent is the
// outcome the executor must produce (state the result, not the
// procedure). All registered tools are available on every step — plans
// no longer scope tools per step (D-029).
type Step struct {
	ID       string     `json:"id"`
	Intent   string     `json:"intent"`
	Status   StepStatus `json:"status"`
	Result   string     `json:"result,omitempty"`
	Error    string     `json:"error,omitempty"`
	Attempts int        `json:"attempts,omitempty"`
	// Finished records the `finished` boolean the LLM passed to
	// step.finish. When true, the executor short-circuits any
	// remaining steps and jumps to synth — the LLM is attesting that
	// the whole user request is satisfied by what has run so far.
	Finished bool `json:"finished,omitempty"`
}

// Plan is the turn-scoped artifact the planner produces. Used both
// for execution control and for the user-facing announcement.
type Plan struct {
	Goal     string `json:"goal"`
	Steps    []Step `json:"steps"`
	StepsRun int    `json:"-"` // total executor LLM iterations across all steps
}

// Next returns a pointer to the next pending step, or nil if none
// remain. Mutations through the returned pointer write back to p.
func (p *Plan) Next() *Step {
	if p == nil {
		return nil
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Status == StepPending {
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

const (
	statusEmojiPending = "⏳"
	statusEmojiRunning = "🔄"
	statusEmojiDone    = "✅"
	statusEmojiFailed  = "❌"
	statusEmojiSkipped = "⏭️"
)

// emoji returns the unicode marker for a step's current status.
func (s *Step) emoji() string {
	switch s.Status {
	case StepRunning:
		return statusEmojiRunning
	case StepDone:
		return statusEmojiDone
	case StepFailed:
		return statusEmojiFailed
	case StepSkipped:
		return statusEmojiSkipped
	default:
		return statusEmojiPending
	}
}

// RenderAnnouncement returns the initial user-facing plan message. The
// loop sends this immediately after the planner commits and stores the
// returned message ID on the Turn so subsequent status edits land here.
func (p *Plan) RenderAnnouncement() string {
	if p == nil || len(p.Steps) == 0 {
		return ""
	}
	return p.renderUserMessage()
}

// RenderStatus returns the user-facing plan message reflecting current
// step statuses. Same layout as the announcement; only the emojis change.
func (p *Plan) RenderStatus() string {
	if p == nil || len(p.Steps) == 0 {
		return ""
	}
	return p.renderUserMessage()
}

func (p *Plan) renderUserMessage() string {
	var sb strings.Builder
	if goal := strings.TrimSpace(p.Goal); goal != "" {
		fmt.Fprintf(&sb, "**Working on:** %s\n\n", goal)
	} else {
		sb.WriteString("**Working on it.**\n\n")
	}
	for i, s := range p.Steps {
		fmt.Fprintf(&sb, "%s %d. %s\n", s.emoji(), i+1, strings.TrimSpace(s.Intent))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Render returns the <plan> block for injection into LLM prompts where
// the plan is part of the context. The structured JSON form so the
// model can reason over goal, ordering, statuses, and per-step results.
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
// status. Idempotent.
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
