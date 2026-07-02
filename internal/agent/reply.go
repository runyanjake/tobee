package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// ReplySender delivers a message to a channel and returns the platform
// message ID of the message it sent. Integrations register one for
// their own integration name so the agent can deliver without knowing
// how the transport works. The returned ID is needed by the agent to
// edit the plan announcement in place as steps progress.
type ReplySender func(ctx context.Context, channel, thread, text string) (messageID string, err error)

// MessageEditor mutates an existing message's content. Integrations
// register one for their integration name when the transport supports
// in-place edits (Discord does). When no editor is registered, callers
// fall back to sending a new message.
type MessageEditor func(ctx context.Context, channel, messageID, text string) error

// Reactor adds or removes a single emoji reaction on a message.
// Integrations register one when the transport supports reactions
// (Discord does); add=false removes the bot's own reaction. Used by the
// loop to give tactile progress feedback on the inbound message.
type Reactor func(ctx context.Context, channel, messageID, emoji string, add bool) error

// Replies is a lookup table of integration name → sender (+ optional
// editor + reactor). The agent looks up by integration to deliver
// replies, update plan-announcement messages, and react to the inbound
// message as the turn progresses.
type Replies struct {
	mu       sync.RWMutex
	senders  map[string]ReplySender
	editors  map[string]MessageEditor
	reactors map[string]Reactor
}

func NewReplies() *Replies {
	return &Replies{
		senders:  make(map[string]ReplySender),
		editors:  make(map[string]MessageEditor),
		reactors: make(map[string]Reactor),
	}
}

// Register associates an integration name with its reply sender.
func (r *Replies) Register(integration string, s ReplySender) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.senders[integration] = s
}

// RegisterEditor associates an integration name with an in-place
// message editor. Optional; not all transports support edits.
func (r *Replies) RegisterEditor(integration string, e MessageEditor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.editors[integration] = e
}

// RegisterReactor associates an integration name with an emoji reactor.
// Optional; not all transports support reactions.
func (r *Replies) RegisterReactor(integration string, rc Reactor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactors[integration] = rc
}

// Send delivers text via the sender registered for integration.
// Returns the platform message ID of the sent message (empty if the
// sender does not produce one or on error).
func (r *Replies) Send(ctx context.Context, integration, channel, thread, text string) (string, error) {
	r.mu.RLock()
	s, ok := r.senders[integration]
	r.mu.RUnlock()
	if !ok {
		slog.Error("reply: no sender registered", "integration", integration)
		return "", fmt.Errorf("no sender for %q", integration)
	}
	id, err := s(ctx, channel, thread, text)
	if err != nil {
		slog.Error("reply: send failed", "integration", integration, "err", err)
		return "", err
	}
	return id, nil
}

// Edit replaces the content of a previously-sent message. Returns
// the underlying error if the editor is missing or fails — callers
// usually log and continue rather than abort the turn over a missed
// status update.
func (r *Replies) Edit(ctx context.Context, integration, channel, messageID, text string) error {
	r.mu.RLock()
	e, ok := r.editors[integration]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no editor for %q", integration)
	}
	if err := e(ctx, channel, messageID, text); err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	return nil
}

// React adds an emoji reaction to messageID. Returns the underlying
// error if no reactor is registered or the call fails — callers log and
// continue rather than abort a turn over a missed reaction.
func (r *Replies) React(ctx context.Context, integration, channel, messageID, emoji string) error {
	return r.reaction(ctx, integration, channel, messageID, emoji, true)
}

// RemoveReaction removes the bot's own emoji reaction from messageID.
func (r *Replies) RemoveReaction(ctx context.Context, integration, channel, messageID, emoji string) error {
	return r.reaction(ctx, integration, channel, messageID, emoji, false)
}

func (r *Replies) reaction(ctx context.Context, integration, channel, messageID, emoji string, add bool) error {
	r.mu.RLock()
	rc, ok := r.reactors[integration]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no reactor for %q", integration)
	}
	if err := rc(ctx, channel, messageID, emoji, add); err != nil {
		return fmt.Errorf("react: %w", err)
	}
	return nil
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
		if _, ok := r.editors[n]; ok {
			names += "+edit"
		}
	}
	return fmt.Sprintf("reply senders: %s", names)
}
