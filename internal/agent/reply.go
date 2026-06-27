package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// ReplySender delivers a final assistant message back to the source of an
// envelope. Integrations register one for their own integration name so the
// agent can reply without knowing how the transport works.
type ReplySender func(ctx context.Context, channel, thread, text string) error

// Replies is a lookup table of integration name → ReplySender.
type Replies struct {
	mu      sync.RWMutex
	senders map[string]ReplySender
}

func NewReplies() *Replies {
	return &Replies{senders: make(map[string]ReplySender)}
}

// Register associates an integration name with its reply sender.
func (r *Replies) Register(integration string, s ReplySender) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.senders[integration] = s
}

// Send delivers text via the sender registered for integration. If no sender
// exists the event is logged and dropped — this should never happen in
// normal operation (every inbound integration ought to register one).
func (r *Replies) Send(ctx context.Context, integration, channel, thread, text string) {
	r.mu.RLock()
	s, ok := r.senders[integration]
	r.mu.RUnlock()
	if !ok {
		slog.Error("reply: no sender registered", "integration", integration)
		return
	}
	if err := s(ctx, channel, thread, text); err != nil {
		slog.Error("reply: send failed", "integration", integration, "err", err)
	}
}

// Summary returns the list of registered integration names, for diagnostics.
func (r *Replies) Summary() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.senders) == 0 {
		return "(none)"
	}
	names := ""
	for n := range r.senders {
		if names != "" {
			names += ", "
		}
		names += n
	}
	return fmt.Sprintf("reply senders: %s", names)
}
