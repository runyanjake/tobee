package agent

import (
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/sandboxfs"
	"github.com/runyanjake/tobee/internal/scope"
	"github.com/runyanjake/tobee/internal/workspace"
)

// ContextBuilder assembles the prompt fragments shared by every phase of
// a turn. The agent loop calls ComposeSystem with a phase-specific persona
// and (when running the executor or synthesizer) the current plan; the
// data sections are identical across phases so memory, workspace, and
// context stay consistent end-to-end.
type ContextBuilder struct {
	Persona   string           // executor persona; default when no override given
	Memory    *sandboxfs.FS    // typed filesystem for always-injected files
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
// persona is the phase's persona text (act-loop persona or synthesizer
// persona). The section order is the prefix-cache contract (D-017).
func (b *ContextBuilder) ComposeSystem(env integrations.Envelope, persona string) string {
	var sb strings.Builder

	if persona != "" {
		sb.WriteString(persona)
		sb.WriteString("\n\n")
	}

	// Workspace-areas list sits right after persona: it's the most stable
	// section of the prompt across turns (config-time only) and so belongs
	// at the front per D-017's prefix-cache contract.
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

	if b.Memory != nil {
		userDir := scope.FromEnvelope(env).Dir()
		b.writeMem(&sb, "shared/INDEX.md")
		if userDir != "" {
			b.writeMem(&sb, userDir+"/INDEX.md")
			b.writeMem(&sb, userDir+"/user.md")
			b.writeMem(&sb, userDir+"/preferences.md")
		}
	}

	if summary := b.Sessions.ReadSummary(env.Key()); summary != "" {
		fmt.Fprintf(&sb, "<session-summary>\n%s\n</session-summary>\n\n", summary)
	}

	return strings.TrimRight(sb.String(), "\n")
}

func (b *ContextBuilder) writeMem(sb *strings.Builder, path string) {
	if !b.Memory.Exists(path) {
		return
	}
	body, err := b.Memory.Read(path)
	if err != nil || body == "" {
		return
	}
	fmt.Fprintf(sb, "<memory path=%q>\n%s\n</memory>\n\n", path, body)
}
