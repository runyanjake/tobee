package agent

import (
	"context"
	"tobee/internal/msg"
)

// Message is an alias for msg.Message so callers only need to import agent.
type Message = msg.Message

// WithMessage attaches m to ctx so downstream handlers can retrieve it.
func WithMessage(ctx context.Context, m Message) context.Context {
	return msg.WithMessage(ctx, m)
}

// MessageFrom retrieves the Message from ctx. Returns false if none is set.
func MessageFrom(ctx context.Context) (Message, bool) {
	return msg.MessageFrom(ctx)
}
