package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcphost"
)

type ServerLister interface {
	Servers() []mcphost.ServerInfo
}

// ContextBuilder builds the system message, sent once per turn; phase directives ride in state templates (D-029).
type ContextBuilder struct {
	Persona string       // prompts/system/*.md concatenated
	Servers ServerLister // nil = none

	// Now overrides time.Now so tests get a deterministic system message.
	Now func() time.Time
}

func (b *ContextBuilder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// ComposeSystem renders persona, <servers> (D-033), then the per-turn <context>.
// Stable parts come first for prefix caching (D-017).
func (b *ContextBuilder) ComposeSystem(ev event.Event) string {
	var sb strings.Builder

	if b.Persona != "" {
		sb.WriteString(b.Persona)
		sb.WriteString("\n\n")
	}

	if b.Servers != nil {
		if block := renderServers(b.Servers.Servers()); block != "" {
			sb.WriteString(block)
			sb.WriteString("\n\n")
		}
	}

	// The model has no clock; without this it dates from its training cutoff.
	now := b.now()
	fmt.Fprintf(&sb, "<context>now=%s (%s)", now.Format(time.RFC3339), now.Format("Monday, 2 January 2006"))
	fmt.Fprintf(&sb, " source=%s kind=%s", ev.Source, ev.Kind)
	fmt.Fprintf(&sb, " connector=%s channel=%s", ev.Origin.Connector, ev.Origin.Channel)
	if ev.Origin.Thread != "" {
		fmt.Fprintf(&sb, " thread=%s", ev.Origin.Thread)
	}
	if ev.Actor.ID != "" {
		if ev.Actor.Name != "" {
			fmt.Fprintf(&sb, " user=%s id=%s", ev.Actor.Name, ev.Actor.ID)
		} else {
			fmt.Fprintf(&sb, " user=%s", ev.Actor.ID)
		}
	} else {
		// memory's scope="user" and user_ask fail without a user; say so up front.
		sb.WriteString(" user=none")
	}
	sb.WriteString("</context>")

	return sb.String()
}

// Untrusted servers get a name and tool list only, never their instructions (D-038).
func renderServers(servers []mcphost.ServerInfo) string {
	if len(servers) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<servers>\n")
	for i, s := range servers {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "## %s", s.Name)
		if !s.Trusted {
			sb.WriteString(" (external)")
		}
		sb.WriteByte('\n')
		if s.Instructions != "" {
			sb.WriteString(s.Instructions)
			sb.WriteByte('\n')
		}
		if len(s.Tools) > 0 {
			fmt.Fprintf(&sb, "Tools: %s\n", strings.Join(s.Tools, ", "))
		}
	}
	sb.WriteString("</servers>")
	return sb.String()
}
