package core

import "context"

// Message is a generic inbound message from any integration.
type Message struct {
	Integration string // e.g. "discord"
	SessionID   string // opaque session identifier; interpreted by the integration's reply handler
	Content     string // raw user text
}

type ctxKey struct{}

// WithMessage attaches msg to ctx so downstream handlers can retrieve it.
func WithMessage(ctx context.Context, msg Message) context.Context {
	return context.WithValue(ctx, ctxKey{}, msg)
}

// MessageFrom retrieves the Message from ctx. Returns false if none is set.
func MessageFrom(ctx context.Context) (Message, bool) {
	msg, ok := ctx.Value(ctxKey{}).(Message)
	return msg, ok
}
