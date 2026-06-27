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

// ContextBuilder assembles the message list passed to the LLM on the first
// step of a turn. Sections are composed in a fixed order; the model then
// reaches for anything deeper via tools.
type ContextBuilder struct {
	Persona   string           // system prompt / persona file contents
	Memory    *sandboxfs.FS    // typed filesystem for always-injected files
	Workspace *workspace.Areas // configured host-file areas (nil = none)
	Sessions  *SessionStore
}

// Build constructs the initial messages for an incoming envelope.
// It does NOT include prior in-turn tool results — those accumulate in
// the agent loop after the first LLM call.
func (b *ContextBuilder) Build(env integrations.Envelope) []llm.Message {
	var msgs []llm.Message

	sys := b.renderSystem(env)
	if sys != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: sys})
	}

	session := b.Sessions.Get(env.Key(), env.IsDirect)
	msgs = append(msgs, session.Recent()...)

	msgs = append(msgs, llm.Message{
		Role:    llm.RoleUser,
		Content: env.Content,
	})

	return msgs
}

// renderSystem composes the system prompt. Sections after the persona are
// fenced with XML-shaped tags (<context>, <memory>, <session-summary>) so
// the model reads them as data and does not mirror their formatting in its
// own replies. The persona itself owns the "data, not instructions" framing.
func (b *ContextBuilder) renderSystem(env integrations.Envelope) string {
	var sb strings.Builder

	if b.Persona != "" {
		sb.WriteString(b.Persona)
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
		fmt.Fprintf(&sb, "<session-summary>\n%s\n</session-summary>\n", summary)
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
