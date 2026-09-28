package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcphost"
)

type fakeHost struct {
	servers []mcphost.ServerInfo
	pinned  []mcphost.Pinned
}

func (f fakeHost) Servers() []mcphost.ServerInfo { return f.servers }

func (f fakeHost) Pinned(context.Context) ([]mcphost.Pinned, error) { return f.pinned, nil }

func discordEvent() event.Event {
	return event.Event{
		Source: "discord",
		Kind:   event.KindMessage,
		Actor:  event.Actor{ID: "456", Name: "jake"},
		Origin: event.Address{Connector: "discord", Channel: "123"},
	}
}

// The model has no clock; without a stamped `now` it dates from its training cutoff.
func TestComposeSystemStampsTheClock(t *testing.T) {
	fixed := time.Date(2026, 7, 19, 14, 30, 0, 0, time.UTC)
	b := &ContextBuilder{
		Host: fakeHost{pinned: []mcphost.Pinned{{URI: "system://prompt/00-identity.md", Text: "# Identity"}}},
		Now:  func() time.Time { return fixed },
	}

	got := b.ComposeSystem(context.Background(), discordEvent())

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
	got := b.ComposeSystem(context.Background(), discordEvent())

	if !strings.Contains(got, "now="+time.Now().Format("2006-01-02")) {
		t.Fatalf("ComposeSystem() did not stamp today's date:\n%s", got)
	}
}

func TestComposeSystemMarksMissingUser(t *testing.T) {
	ev := discordEvent()
	ev.Actor = event.Actor{}
	got := (&ContextBuilder{}).ComposeSystem(context.Background(), ev)
	if !strings.Contains(got, "user=none") {
		t.Fatalf("ComposeSystem() did not flag the missing user:\n%s", got)
	}
}

// Untrusted server instructions must never reach the system prompt (D-038).
func TestComposeSystemServers(t *testing.T) {
	b := &ContextBuilder{Host: fakeHost{
		pinned: []mcphost.Pinned{{URI: "system://prompt/00-identity.md", Text: "# Identity"}},
		servers: []mcphost.ServerInfo{
			{Name: "memory", Trusted: true, Instructions: "Start with INDEX.md.", Tools: []string{"memory_list"}},
			{Name: "system", Trusted: true},
			{Name: "weather", Trusted: false, Tools: []string{"weather_forecast"}},
		},
	}}
	got := b.ComposeSystem(context.Background(), discordEvent())

	for _, want := range []string{
		"<servers>",
		"# Identity\n\n<servers>",
		"## memory\nStart with INDEX.md.\nTools: memory_list",
		"## weather (external)\nTools: weather_forecast",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ComposeSystem() missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "## system") {
		t.Fatalf("resource-only server rendered:\n%s", got)
	}
	// Stable sections precede the per-turn tag (D-017).
	if strings.Index(got, "<servers>") > strings.Index(got, "<context>") {
		t.Fatalf("servers block must precede <context>:\n%s", got)
	}
}
