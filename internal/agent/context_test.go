package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcphost"
)

type fakeServers []mcphost.ServerInfo

func (f fakeServers) Servers() []mcphost.ServerInfo { return f }

func discordEvent() event.Event {
	return event.Event{
		Source: "discord",
		Kind:   event.KindMessage,
		Actor:  event.Actor{ID: "456", Name: "jake"},
		Origin: event.Address{Connector: "discord", Channel: "123"},
	}
}

// The model has no clock. Without a stamped `now` it dates timestamps
// from its training cutoff — see D-030.
func TestComposeSystemStampsTheClock(t *testing.T) {
	fixed := time.Date(2026, 7, 19, 14, 30, 0, 0, time.UTC)
	b := &ContextBuilder{
		Persona: "# Identity",
		Now:     func() time.Time { return fixed },
	}

	got := b.ComposeSystem(discordEvent())

	for _, want := range []string{
		"now=2026-07-19T14:30:00Z",
		"Sunday, 19 July 2026",
		"connector=discord",
		"channel=123",
		"user=jake",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ComposeSystem() missing %q:\n%s", want, got)
		}
	}
}

func TestComposeSystemDefaultsToRealClock(t *testing.T) {
	b := &ContextBuilder{}
	got := b.ComposeSystem(discordEvent())

	if !strings.Contains(got, "now="+time.Now().Format("2006-01-02")) {
		t.Fatalf("ComposeSystem() did not stamp today's date:\n%s", got)
	}
}

func TestComposeSystemMarksMissingUser(t *testing.T) {
	ev := discordEvent()
	ev.Actor = event.Actor{}
	got := (&ContextBuilder{}).ComposeSystem(ev)
	if !strings.Contains(got, "user=none") {
		t.Fatalf("ComposeSystem() did not flag the missing user:\n%s", got)
	}
}

// Untrusted server instructions are third-party text; they must never
// reach the system prompt (D-038).
func TestComposeSystemServers(t *testing.T) {
	b := &ContextBuilder{Servers: fakeServers{
		{Name: "memory", Trusted: true, Instructions: "Start with INDEX.md.", Tools: []string{"memory_read"}},
		{Name: "weather", Trusted: false, Tools: []string{"weather_forecast"}},
	}}
	got := b.ComposeSystem(discordEvent())

	for _, want := range []string{
		"<servers>",
		"## memory\nStart with INDEX.md.\nTools: memory_read",
		"## weather (external)\nTools: weather_forecast",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ComposeSystem() missing %q:\n%s", want, got)
		}
	}
	// Stable sections precede the per-turn tag (D-017).
	if strings.Index(got, "<servers>") > strings.Index(got, "<context>") {
		t.Fatalf("servers block must precede <context>:\n%s", got)
	}
}
