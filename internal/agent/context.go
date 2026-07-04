package agent

import (
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/workspace"
)

// ContextBuilder assembles the prompt fragments shared by every phase of
// a turn. The system prompt carries the persona (identity, tone, output
// rules) and a small set of turn-local hints: workspace areas, the
// integration/channel/user context, a memory-access reminder, and the
// rolling session summary. Stored knowledge is NOT pre-injected — the
// model fetches memory files on demand via memory.* tools. The session
// ring buffer is what "continues the chat" across turns.
type ContextBuilder struct {
	Persona   string           // persona blob (identity + behaviour + output + safety)
	Workspace *workspace.Areas // configured host-file areas (nil = none)
	Sessions  *SessionStore
}

// ComposeTranscript builds the recent-ring + new-user-message tail. Does
// NOT include a system message — every phase prepends its own.
func (b *ContextBuilder) ComposeTranscript(env integrations.Envelope) []llm.Message {
	session := b.Sessions.Get(env.Key(), env.IsDirect)
	msgs := append([]llm.Message{}, session.Recent()...)
	msgs = append(msgs, llm.Message{
		Role:    llm.RoleUser,
		Content: env.Content,
	})
	return msgs
}

// ComposeSystem renders the system message for one phase of a turn.
// The section order is the prefix-cache contract (D-017).
func (b *ContextBuilder) ComposeSystem(env integrations.Envelope, persona string) string {
	var sb strings.Builder

	if persona != "" {
		sb.WriteString(persona)
		sb.WriteString("\n\n")
	}

	if b.Workspace != nil && b.Workspace.Len() > 0 {
		sb.WriteString("<workspace_areas>\n")
		for _, ar := range b.Workspace.List() {
			fmt.Fprintf(&sb, "- %s", ar.Name)
			if ar.ReadOnly {
				sb.WriteString(" (read-only)")
			}
			if ar.Description != "" {
				fmt.Fprintf(&sb, ": %s", ar.Description)
			}
			sb.WriteByte('\n')
		}
		sb.WriteString("</workspace_areas>\n\n")
	}

	fmt.Fprintf(&sb, "<context>integration=%s channel=%s", env.Integration, env.Channel)
	if env.Thread != "" {
		fmt.Fprintf(&sb, " thread=%s", env.Thread)
	}
	if env.User != "" {
		if env.UserName != "" {
			fmt.Fprintf(&sb, " user=%s id=%s", env.UserName, env.User)
		} else {
			fmt.Fprintf(&sb, " user=%s", env.User)
		}
	}
	sb.WriteString("</context>\n\n")

	sb.WriteString(memoryHint(scope.FromEnvelope(env).HasUser()))
	sb.WriteString("\n\n")

	if summary := b.Sessions.ReadSummary(env.Key()); summary != "" {
		fmt.Fprintf(&sb, "<session-summary>\n%s\n</session-summary>\n\n", summary)
	}

	return strings.TrimRight(sb.String(), "\n")
}

// memoryHint is the small block that replaces the pre-injected memory
// files. It tells the model where its stored knowledge lives and which
// tools reach it — the content itself is fetched on demand.
func memoryHint(hasUser bool) string {
	var sb strings.Builder
	sb.WriteString("<memory>\n")
	sb.WriteString("Your stored knowledge is not in this prompt. Fetch what you need with tool calls:\n")
	sb.WriteString("- `memory.read({path, scope})` — read a specific file. scope=\"user\" for the current user, \"shared\" for cross-user.\n")
	sb.WriteString("- `memory.search({query, scope})` — case-insensitive substring hits across a scope (default \"both\").\n")
	sb.WriteString("- `memory.list({dir, scope})` — enumerate files under a scope.\n")
	sb.WriteString("Start with `memory.read({path: \"INDEX.md\", scope: \"user\"})` for the user's table of contents, or scope \"shared\" for cross-user knowledge.\n")
	if !hasUser {
		sb.WriteString("No user is attached to this turn; only scope=\"shared\" is available.\n")
	}
	sb.WriteString("</memory>")
	return sb.String()
}
