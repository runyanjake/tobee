package agent

import "github.com/runyanjake/tobee/internal/llm"

// Conversation is one task's chat; the whole list is resent on each stateless call.
type Conversation struct {
	Messages []llm.Message

	// turnStart is where this turn's messages begin, after the system
	// prompt and session history; harness marks directives and nudges,
	// which are never saved to the session.
	turnStart int
	harness   map[int]bool

	logged int // Messages already written to the debug log (see logNewMessages)
}

func NewConversation(systemPrompt string, history []llm.Message) *Conversation {
	msgs := make([]llm.Message, 0, 8+len(history))
	if systemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: systemPrompt})
	}
	msgs = append(msgs, history...)
	return &Conversation{Messages: msgs, turnStart: len(msgs), harness: map[int]bool{}}
}

// Append adds a message; never mutate earlier positions, every call resends them.
func (c *Conversation) Append(m llm.Message) {
	c.Messages = append(c.Messages, m)
}

// AppendHarness adds a message the harness wrote rather than the user or model.
func (c *Conversation) AppendHarness(m llm.Message) {
	c.harness[len(c.Messages)] = true
	c.Messages = append(c.Messages, m)
}

// Bytes is the rough size of the conversation, used as a backstop against a
// turn growing past the model's context window (D-057).
func (c *Conversation) Bytes() int {
	n := 0
	for _, m := range c.Messages {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments) + len(tc.Function.Name)
		}
	}
	return n
}

// TurnMessages is this turn's user, model, and tool messages, without
// harness directives: what gets saved to the session.
func (c *Conversation) TurnMessages() []llm.Message {
	var out []llm.Message
	for i := c.turnStart; i < len(c.Messages); i++ {
		if !c.harness[i] {
			out = append(out, c.Messages[i])
		}
	}
	return out
}
