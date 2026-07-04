package agent

import (
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/workspace"
)

// ContextBuilder builds the one system message that seeds every
// per-request Conversation (D-029). The chat runs one continuous
// conversation from planner through synth — the system prompt is sent
// once at Messages[0], not re-sent per phase. Phase-specific
// instructions ride in as user-role state templates (prompts/state/).
type ContextBuilder struct {
	Persona   string           // system prompt blob (prompts/system/*.md concatenated)
	Workspace *workspace.Areas // configured host-file areas (nil = none)
}

// ComposeSystem renders the system message once per request. Contains
// the loaded system prompt, the workspace-areas list (if any), a small
// per-turn context tag, and the memory-tools hint. This message sits
// at Messages[0] for the whole request.
func (b *ContextBuilder) ComposeSystem(env integrations.Envelope) string {
	var sb strings.Builder

	if b.Persona != "" {
		sb.WriteString(b.Persona)
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

	return strings.TrimRight(sb.String(), "\n")
}

// memoryHint is the small block that names the memory paths and tools.
// The model tool-calls memory.* to fetch actual content on demand.
func memoryHint(hasUser bool) string {
	var sb strings.Builder
	sb.WriteString("<memory>\n")
	sb.WriteString("Your stored knowledge is not in this prompt. Each turn is a fresh chat with no memory of prior turns — anything you need to remember across messages must be written to and read from these tools:\n")
	sb.WriteString("- `memory.read({path, scope})` — read a specific file. scope=\"user\" for the current user, \"shared\" for cross-user.\n")
	sb.WriteString("- `memory.search({query, scope})` — case-insensitive substring hits across a scope (default \"both\").\n")
	sb.WriteString("- `memory.list({dir, scope})` — enumerate files under a scope.\n")
	sb.WriteString("- `memory.write({path, content, scope})` — create or overwrite a file. Save preferences, decisions, and facts here.\n")
	sb.WriteString("- `memory.append({path, content, scope})` — append to a file, creating it if needed.\n")
	sb.WriteString("Start with `memory.read({path: \"INDEX.md\", scope: \"user\"})` for the user's table of contents.\n")
	if !hasUser {
		sb.WriteString("No user is attached to this turn; only scope=\"shared\" is available.\n")
	}
	sb.WriteString("</memory>")
	return sb.String()
}
