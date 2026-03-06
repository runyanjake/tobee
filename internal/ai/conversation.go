package ai

// Conversation manages an ordered sequence of chat messages.
// It is designed to be built incrementally and cloned for branching —
// a base conversation seeded with system context can be cloned once per
// incoming request so each request gets a fresh but pre-seeded history.
type Conversation struct {
	messages []Message
}

func NewConversation() *Conversation {
	return &Conversation{}
}

// System appends a system message. Returns the conversation for chaining.
func (c *Conversation) System(content string) *Conversation {
	c.messages = append(c.messages, Message{Role: "system", Content: content})
	return c
}

// User appends a user message. Returns the conversation for chaining.
func (c *Conversation) User(content string) *Conversation {
	c.messages = append(c.messages, Message{Role: "user", Content: content})
	return c
}

// Assistant appends an assistant message. Returns the conversation for chaining.
// Use this to inject acknowledged responses when seeding context, or to record
// AI replies when building a multi-turn history.
func (c *Conversation) Assistant(content string) *Conversation {
	c.messages = append(c.messages, Message{Role: "assistant", Content: content})
	return c
}

// Messages returns a snapshot of the message slice, safe to pass to Client.Chat.
func (c *Conversation) Messages() []Message {
	msgs := make([]Message, len(c.messages))
	copy(msgs, c.messages)
	return msgs
}

// Clone returns a deep copy of the conversation. The clone can be extended
// independently without affecting the original — useful for branching a
// shared base conversation per request.
func (c *Conversation) Clone() *Conversation {
	msgs := make([]Message, len(c.messages))
	copy(msgs, c.messages)
	return &Conversation{messages: msgs}
}

// Len returns the number of messages in the conversation.
func (c *Conversation) Len() int {
	return len(c.messages)
}
