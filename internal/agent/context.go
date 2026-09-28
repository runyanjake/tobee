package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcphost"
)

// ServerLister is the slice of the MCP host the context builder reads.
type ServerLister interface {
	Servers() []mcphost.ServerInfo
}

// ContextBuilder builds the one system message that seeds every
// per-request Conversation (D-029). The chat runs one continuous
// conversation from planner through synth — the system prompt is sent
// once at Messages[0], not re-sent per phase. Phase-specific
// instructions ride in as user-role state templates (prompts/state/).
type ContextBuilder struct {
	Persona string       // system prompt blob (prompts/system/*.md concatenated)
	Servers ServerLister // connected MCP servers (nil = none)

	// Now is the clock stamped into the per-turn context tag. nil means
	// time.Now; tests set it for a deterministic system message.
	Now func() time.Time
}

func (b *ContextBuilder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// ComposeSystem renders the system message once per request: the system
// prompt, a <servers> block built from the MCP host (D-033), and a small
// per-turn <context> tag. Stable sections come first for prefix caching
// (D-017).
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

	// The model has no clock of its own. Without this it dates timestamps
	// from its training cutoff — a `since`/`at` it invents lands years off.
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
		// memory's scope="user" and user_ask fail without a user; say so
		// up front rather than let the model find out by error.
		sb.WriteString(" user=none")
	}
	sb.WriteString("</context>")

	return sb.String()
}

// renderServers lists every connected MCP server with its instructions.
// Untrusted servers get a name and tool list only: their instructions are
// third-party text and do not belong in the system prompt (D-038).
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
