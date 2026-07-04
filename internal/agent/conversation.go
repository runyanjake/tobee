package agent

import "github.com/runyanjake/tobee/internal/llm"

// Conversation is the growing chat state for one Discord message from
// receipt through delivery. Every phase (plan, execute per step, synth)
// appends to the same Messages list — LM Studio is stateless on the
// wire, so we resend the whole list on each call, but from our code's
// view this is one continuous conversation.
//
// SurfacedKnowledge is a stub for a future integration (web search /
// file search) that will accumulate durable facts separately from the
// raw message tail. Empty today; kept on the struct so state templates
// can reference `{{.SurfacedKnowledge}}` and be no-op if nothing is
// there yet.
type Conversation struct {
	Messages          []llm.Message
	Plan              *Plan
	StepCursor        int      // index of the step being executed (0-indexed)
	SurfacedKnowledge []string // stub — populated by future web/file search hooks
	Finished          bool     // set when a step.finish signals the whole turn is done
}

// NewConversation seeds the chat with a system message and returns
// the empty container. All subsequent phases append to Messages.
func NewConversation(systemPrompt string) *Conversation {
	msgs := make([]llm.Message, 0, 8)
	if systemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: systemPrompt})
	}
	return &Conversation{Messages: msgs}
}

// Append records one message into the conversation. Every LLM call
// (plan, per-step, synth) reads back the whole Messages slice; do not
// mutate positions in place after appending.
func (c *Conversation) Append(m llm.Message) {
	c.Messages = append(c.Messages, m)
}
