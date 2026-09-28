package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// ContextHost is the part of the MCP host the system message is built from.
type ContextHost interface {
	Servers() []mcphost.ServerInfo
	Pinned(ctx context.Context) ([]mcphost.Pinned, error)
}

// ContextBuilder builds the system message, sent once per turn; phase directives ride in state templates (D-029).
type ContextBuilder struct {
	Host ContextHost // nil = no pinned resources and no servers

	// Now overrides time.Now so tests get a deterministic system message.
	Now func() time.Time
}

func (b *ContextBuilder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// ComposeSystem renders pinned resources (D-042), <servers> (D-033), then the
// per-turn <context>. Stable parts come first for prefix caching (D-017).
func (b *ContextBuilder) ComposeSystem(ctx context.Context, ev event.Event) string {
	var sb strings.Builder

	if b.Host != nil {
		pinned, err := b.Host.Pinned(ctx)
		if err != nil {
			telemetry.Logger(ctx).Warn("agent: pinned resource read failed; system prompt is partial", "err", err)
		}
		for _, p := range pinned {
			sb.WriteString(p.Text)
			sb.WriteString("\n\n")
		}
		if block := renderServers(b.Host.Servers()); block != "" {
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
	var sb strings.Builder
	for _, s := range servers {
		// Resource-only servers (system) have nothing for the model to call.
		if s.Instructions == "" && len(s.Tools) == 0 {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("<servers>\n")
		} else {
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
	if sb.Len() == 0 {
		return ""
	}
	sb.WriteString("</servers>")
	return sb.String()
}
