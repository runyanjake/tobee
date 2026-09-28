// Package delivery routes output back to the channel an event came from.
// Each connector that can carry replies registers a Channel under its
// name; the agent delivers by event.Address without knowing the transport.
//
// Replying to the origin is not a tool: it is the default end of every
// turn and is done in code, so a reply cannot be skipped or reworded by
// the model (D-035). Messages anywhere else go through tools.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/runyanjake/tobee/internal/event"
)

// ErrUnsupported is returned when a connector cannot perform an operation
// (e.g. editing a sent email).
var ErrUnsupported = errors.New("not supported by this connector")

// Channel sends text to an address and returns the connector's id for
// the sent message ("" if it has none).
type Channel interface {
	Send(ctx context.Context, to event.Address, text string) (messageID string, err error)
}

// Editor is implemented by channels that can replace a sent message's text.
type Editor interface {
	Edit(ctx context.Context, to event.Address, messageID, text string) error
}

// Reactor is implemented by channels that can add or remove an emoji
// reaction on a message.
type Reactor interface {
	React(ctx context.Context, to event.Address, messageID, emoji string, add bool) error
}

// Router maps connector names to channels.
type Router struct {
	mu       sync.RWMutex
	channels map[string]Channel
}

func NewRouter() *Router {
	return &Router{channels: make(map[string]Channel)}
}

// Register makes c the channel for connector, replacing any previous one.
func (r *Router) Register(connector string, c Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[connector] = c
}

// Unregister removes a connector's channel.
func (r *Router) Unregister(connector string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.channels, connector)
}

func (r *Router) get(connector string) (Channel, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.channels[connector]
	if !ok {
		return nil, fmt.Errorf("delivery: no channel for %q", connector)
	}
	return c, nil
}

// Send delivers text to an address.
func (r *Router) Send(ctx context.Context, to event.Address, text string) (string, error) {
	c, err := r.get(to.Connector)
	if err != nil {
		return "", err
	}
	id, err := c.Send(ctx, to, text)
	if err != nil {
		return "", fmt.Errorf("delivery: send via %s: %w", to.Connector, err)
	}
	return id, nil
}

// CanEdit reports whether the connector supports in-place edits. The
// agent announces a plan only where it can update it afterwards.
func (r *Router) CanEdit(connector string) bool {
	c, err := r.get(connector)
	if err != nil {
		return false
	}
	_, ok := c.(Editor)
	return ok
}

// Edit replaces the text of a sent message.
func (r *Router) Edit(ctx context.Context, to event.Address, messageID, text string) error {
	c, err := r.get(to.Connector)
	if err != nil {
		return err
	}
	e, ok := c.(Editor)
	if !ok {
		return ErrUnsupported
	}
	return e.Edit(ctx, to, messageID, text)
}

// React adds (add=true) or removes a reaction on messageID.
func (r *Router) React(ctx context.Context, to event.Address, messageID, emoji string, add bool) error {
	c, err := r.get(to.Connector)
	if err != nil {
		return err
	}
	rc, ok := c.(Reactor)
	if !ok {
		return ErrUnsupported
	}
	return rc.React(ctx, to, messageID, emoji, add)
}

// Names returns the registered connector names, sorted.
func (r *Router) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.channels))
	for n := range r.channels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
