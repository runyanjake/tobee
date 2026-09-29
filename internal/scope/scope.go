// Package scope carries the user identity through a turn; MCP servers can't see the
// turn's context, so it also travels in tools/call `_meta`.
package scope

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/runyanjake/tobee/internal/event"
)

// MetaKey is sent only to trusted servers (D-038).
const MetaKey = "tobee/scope"

// Channel and Thread are routing hints only; Key and Dir stay user-only and
// filesystem-safe. Empty User means no user (e.g. a resource notification).
type UserScope struct {
	Connector string `json:"connector"`
	User      string `json:"user,omitempty"`
	Person    string `json:"person,omitempty"` // connector-independent owner (D-045)
	UserName  string `json:"userName,omitempty"`
	Channel   string `json:"channel"`
	Thread    string `json:"thread,omitempty"`
}

func FromEvent(e event.Event) UserScope {
	return UserScope{
		Connector: e.Origin.Connector,
		User:      e.Actor.ID,
		Person:    e.Actor.Person,
		UserName:  e.Actor.Name,
		Channel:   e.Origin.Channel,
		Thread:    e.Origin.Thread,
	}
}

func (s UserScope) Address() event.Address {
	return event.Address{Connector: s.Connector, Channel: s.Channel, Thread: s.Thread}
}

func (s UserScope) HasUser() bool { return s.User != "" }

// Key returns a sanitized "<connector>/<user>" safe for use as a path component.
// Key is the person's memory key: "jake" for a linked person, or
// "<connector>/<account>" for an unlinked one, matching the older layout.
func (s UserScope) Key() string {
	if !s.HasUser() {
		return ""
	}
	if s.Person == "" {
		return sanitize(s.Connector) + "/" + sanitize(s.User)
	}
	conn, acct, ok := strings.Cut(s.Person, ":")
	if !ok {
		return sanitize(s.Person)
	}
	return sanitize(conn) + "/" + sanitize(acct)
}

// Dir is the memory-relative user tree, e.g. "users/discord/12345".
func (s UserScope) Dir() string {
	if !s.HasUser() {
		return ""
	}
	return "users/" + s.Key()
}

type ctxKey struct{}

func With(ctx context.Context, s UserScope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

func From(ctx context.Context) (UserScope, bool) {
	s, ok := ctx.Value(ctxKey{}).(UserScope)
	return s, ok
}

func (s UserScope) ToMeta() map[string]any {
	return map[string]any{
		"connector": s.Connector,
		"user":      s.User,
		"person":    s.Person,
		"userName":  s.UserName,
		"channel":   s.Channel,
		"thread":    s.Thread,
	}
}

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
