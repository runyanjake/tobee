package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/integrations"
)

// The model has no clock. Without a stamped `now` it dates timestamps
// from its training cutoff — see D-030.
func TestComposeSystemStampsTheClock(t *testing.T) {
	fixed := time.Date(2026, 7, 19, 14, 30, 0, 0, time.UTC)
	b := &ContextBuilder{
		Persona: "# Identity",
		Now:     func() time.Time { return fixed },
	}

	got := b.ComposeSystem(integrations.Envelope{
		Integration: "discord",
		Channel:     "123",
		User:        "456",
		UserName:    "jake",
	})

	for _, want := range []string{
		"now=2026-07-19T14:30:00Z",
		"Sunday, 19 July 2026",
		"integration=discord",
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
	got := b.ComposeSystem(integrations.Envelope{Integration: "discord", Channel: "1"})

	if !strings.Contains(got, "now="+time.Now().Format("2006-01-02")) {
		t.Fatalf("ComposeSystem() did not stamp today's date:\n%s", got)
	}
}
