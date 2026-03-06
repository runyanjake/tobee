// Package msg provides the shared Message type and context helpers used by
// both the core agent loop and integrations (e.g. memory). Keeping it
// separate avoids import cycles between internal/core and integrations/.
package msg

import "context"

// Message is a generic inbound message from any integration.
type Message struct {
	Integration string // e.g. "discord"
	SessionID   string // opaque session identifier; interpreted by the integration's reply handler
	Content     string // raw user text
}

type ctxKey struct{}

// WithMessage attaches msg to ctx so downstream handlers can retrieve it.
func WithMessage(ctx context.Context, m Message) context.Context {
	return context.WithValue(ctx, ctxKey{}, m)
}

// MessageFrom retrieves the Message from ctx. Returns false if none is set.
func MessageFrom(ctx context.Context) (Message, bool) {
	m, ok := ctx.Value(ctxKey{}).(Message)
	return m, ok
}
