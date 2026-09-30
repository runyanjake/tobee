// Package delivery routes output to an event.Address by connector name. The origin
// reply is delivered in code, not by a tool, so the model can't skip it (D-035).
package delivery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/runyanjake/tobee/internal/event"
)

var ErrUnsupported = errors.New("not supported by this connector")

// Send returns the connector's id for the sent message, or "" if it has none.
type Channel interface {
	Send(ctx context.Context, to event.Address, text string) (messageID string, err error)
}

type Editor interface {
	Edit(ctx context.Context, to event.Address, messageID, text string) error
}

type Reactor interface {
	React(ctx context.Context, to event.Address, messageID, emoji string, add bool) error
}

// Mentioner formats a ping for one of the connector's user IDs. Implemented
// only where the platform has a mention syntax; email, for instance, does not.
type Mentioner interface {
	Mention(userID string) string
}

type Router struct {
	mu       sync.RWMutex
	channels map[string]Channel
}

func NewRouter() *Router {
	return &Router{channels: make(map[string]Channel)}
}

// Register replaces any previous channel for connector.
func (r *Router) Register(connector string, c Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[connector] = c
}

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

// The agent announces a plan only where it can edit it afterwards.
func (r *Router) CanEdit(connector string) bool {
	c, err := r.get(connector)
	if err != nil {
		return false
	}
	_, ok := c.(Editor)
	return ok
}

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

// Mention returns the connector's ping for userID, or "" when the connector
// has no mention syntax or the user isn't known.
func (r *Router) Mention(connector, userID string) string {
	if userID == "" {
		return ""
	}
	c, err := r.get(connector)
	if err != nil {
		return ""
	}
	m, ok := c.(Mentioner)
	if !ok {
		return ""
	}
	return m.Mention(userID)
}

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
