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
// a turn. Every LLM call is a fresh, standalone turn — there is no
// cross-turn conversation history and no session summary. The system
// prompt is stable across turns (same bytes → prefix cache hit) and
// carries: identity persona (b.Persona), phase-specific role prompt
// (planner.md, synthesizer.md, or "" for the executor), workspace
// areas, per-turn <context> tags, and the memory-tools primer. The
// user's message is the only transcript sent alongside.
type ContextBuilder struct {
	Persona   string           // system prompt blob (prompts/system/*.md concatenated)
	Workspace *workspace.Areas // configured host-file areas (nil = none)
}

// ComposeTranscript returns the message list sent alongside the system
// prompt for a turn. Only the current user message — no ring buffer,
// no prior turns. Context across tool calls within a turn is handled
// by the executor's per-step transcript; context across turns lives in
// memory files (fetched via memory.* tools), not in the chat.
func (b *ContextBuilder) ComposeTranscript(env integrations.Envelope) []llm.Message {
	return []llm.Message{{
		Role:    llm.RoleUser,
		Content: env.Content,
	}}
}

// ComposeSystem renders the system message for one phase of a turn.
// The identity persona (b.Persona) always leads so every phase — planner,
// executor, synth — sees tobee's identity. Then the phase-specific role
// prompt (planner.md, synthesizer.md, or "" for the executor which uses
// the persona directly) follows.
func (b *ContextBuilder) ComposeSystem(env integrations.Envelope, phaseInstructions string) string {
	var sb strings.Builder

	if b.Persona != "" {
		sb.WriteString(b.Persona)
		sb.WriteString("\n\n")
	}

	if phaseInstructions != "" {
		sb.WriteString(phaseInstructions)
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
// The model tool-calls memory.* to fetch actual content on demand; the
// block itself carries no memory bytes and is byte-stable across turns
// (only branches on whether a user is attached to the envelope).
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
