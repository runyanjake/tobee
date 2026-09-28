package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped" // an earlier step's finished=true short-circuited
)

// Step's Intent is the outcome to produce, not the procedure; every tool is available (D-029).
type Step struct {
	ID       string     `json:"id"`
	Intent   string     `json:"intent"`
	Status   StepStatus `json:"status"`
	Result   string     `json:"result,omitempty"`
	Error    string     `json:"error,omitempty"`
	Attempts int        `json:"attempts,omitempty"`
	// Finished is the model attesting the whole request is satisfied; remaining steps are skipped.
	Finished bool `json:"finished,omitempty"`
}

type Plan struct {
	Goal  string `json:"goal"`
	Steps []Step `json:"steps"`
	// DirectReply is set only when Steps is empty; delivered as-is, skipping execute and synth (D-032).
	DirectReply string `json:"direct_reply,omitempty"`
	StepsRun    int    `json:"-"` // executor LLM iterations across all steps
}

// Next returns the next pending step, or nil; mutations write back to p.
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

// RenderAnnouncement is the initial plan message; later status edits target its ID.
func (p *Plan) RenderAnnouncement() string {
	if p == nil || len(p.Steps) == 0 {
		return ""
	}
	return p.renderUserMessage()
}

// RenderStatus matches the announcement's layout; only the emojis change.
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

// Render returns the <plan> JSON block for LLM prompts.
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

// assignIDs gives every step a stable ID (s1, s2, …) and a default status. Idempotent.
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
