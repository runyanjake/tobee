// Package scope carries the active user identity through a turn.
//
// One UserScope is attached to the per-turn context.Context by the agent
// runtime. In-process MCP servers cannot see that context, so the MCP
// host also sends the scope as request metadata (ToMeta / FromMeta) and
// each built-in server re-attaches it before running a tool handler.
package scope

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/runyanjake/tobee/internal/event"
)

// MetaKey is the `_meta` key the scope travels under on tools/call. It
// is only sent to trusted servers (D-038).
const MetaKey = "tobee/scope"

// UserScope identifies the originating turn: the user, the connector that
// delivered the inbound event, and the channel/thread the reply will land in.
// Empty User means no user is attached (e.g. a resource notification).
// Channel and Thread are pure routing hints — they do not affect Key() /
// Dir(), which remain user-only and safe for filesystem use.
type UserScope struct {
	Connector string `json:"connector"`
	User      string `json:"user,omitempty"`
	UserName  string `json:"userName,omitempty"`
	Channel   string `json:"channel"`
	Thread    string `json:"thread,omitempty"`
}

// FromEvent derives a scope from an inbound event.
func FromEvent(e event.Event) UserScope {
	return UserScope{
		Connector: e.Origin.Connector,
		User:      e.Actor.ID,
		UserName:  e.Actor.Name,
		Channel:   e.Origin.Channel,
		Thread:    e.Origin.Thread,
	}
}

// Address is the delivery address of the turn this scope belongs to.
func (s UserScope) Address() event.Address {
	return event.Address{Connector: s.Connector, Channel: s.Channel, Thread: s.Thread}
}

// HasUser reports whether the scope identifies a specific user.
func (s UserScope) HasUser() bool { return s.User != "" }

// Key returns a sanitized "<connector>/<user>" identifier safe for
// use as a filesystem path component. Characters outside [a-zA-Z0-9-_]
// are replaced with '_'.
func (s UserScope) Key() string {
	if !s.HasUser() {
		return ""
	}
	return sanitize(s.Connector) + "/" + sanitize(s.User)
}

// Dir returns the memory.FS-relative directory for this scope's user
// tree, e.g. "users/discord/12345". Returns "" if no user is attached.
func (s UserScope) Dir() string {
	if !s.HasUser() {
		return ""
	}
	return "users/" + s.Key()
}

type ctxKey struct{}

// With attaches s to ctx.
func With(ctx context.Context, s UserScope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// From extracts the scope attached to ctx. The second return is false
// if no scope was attached.
func From(ctx context.Context) (UserScope, bool) {
	s, ok := ctx.Value(ctxKey{}).(UserScope)
	return s, ok
}

// ToMeta renders s as a `_meta` value.
func (s UserScope) ToMeta() map[string]any {
	return map[string]any{
		"connector": s.Connector,
		"user":      s.User,
		"userName":  s.UserName,
		"channel":   s.Channel,
		"thread":    s.Thread,
	}
}

// FromMeta reads a scope back out of a request's `_meta`. The second
// return is false when none was sent.
func FromMeta(meta map[string]any) (UserScope, bool) {
	raw, ok := meta[MetaKey]
	if !ok {
		return UserScope{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return UserScope{}, false
	}
	var s UserScope
	if err := json.Unmarshal(b, &s); err != nil {
		return UserScope{}, false
	}
	return s, true
}

func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
