package agent

import "github.com/runyanjake/tobee/internal/llm"

// Conversation is one task's chat; the whole list is resent on each stateless call.
type Conversation struct {
	Messages []llm.Message

	logged int // Messages already written to the debug log (see logNewMessages)
}

func NewConversation(systemPrompt string) *Conversation {
	msgs := make([]llm.Message, 0, 8)
	if systemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: systemPrompt})
	}
	return &Conversation{Messages: msgs}
}

// Append adds a message; never mutate earlier positions, every call resends them.
func (c *Conversation) Append(m llm.Message) {
	c.Messages = append(c.Messages, m)
}
