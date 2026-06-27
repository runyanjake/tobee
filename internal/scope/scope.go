// Package scope carries the active user identity through a turn.
//
// One UserScope is attached to the per-turn context.Context by the agent
// loop; downstream tool handlers, reporters, and abilities read it back
// to route memory writes, filter reports, and label outputs.
//
// Scope is intentionally context-bound, not a shared mutable singleton —
// the serial worker model (D-005) makes either work, but context is the
// idiomatic Go choice and survives any future move away from serial.
package scope

import (
	"context"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
)

// UserScope identifies the originating turn: the user, the integration that
// delivered the inbound event, and the channel/thread the reply will land in.
// Empty User means no user is attached (e.g. scheduler ticks). Channel and
// Thread are pure routing hints — they do not affect Key() / Dir(), which
// remain user-only and safe for filesystem use.
type UserScope struct {
	Integration string
	User        string
	UserName    string
	Channel     string
	Thread      string
}

// FromEnvelope derives a scope from an inbound envelope. Returns the
// zero value if the envelope has no user attached.
func FromEnvelope(e integrations.Envelope) UserScope {
	return UserScope{
		Integration: e.Integration,
		User:        e.User,
		UserName:    e.UserName,
		Channel:     e.Channel,
		Thread:      e.Thread,
	}
}

// HasUser reports whether the scope identifies a specific user.
func (s UserScope) HasUser() bool { return s.User != "" }

// Key returns a sanitized "<integration>/<user>" identifier safe for
// use as a filesystem path component. Characters outside [a-zA-Z0-9-_]
// are replaced with '_'.
func (s UserScope) Key() string {
	if !s.HasUser() {
		return ""
	}
	return sanitize(s.Integration) + "/" + sanitize(s.User)
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
