package mcphost

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

func (h *Host) Reporter() abilities.Reporter { return hostReporter{h: h} }

type hostReporter struct{ h *Host }

func (r hostReporter) Name() string { return "mcp" }

// The host keeps no per-call history, so `since` does not narrow the lifetime counters.
func (r hostReporter) Render(_ context.Context, _ time.Time) (string, string) {
	servers := r.h.Servers()
	r.h.mu.RLock()
	defer r.h.mu.RUnlock()

	var full strings.Builder
	calls, errs := 0, 0
	full.WriteString("Doing:\n")
	for _, s := range servers {
		trust := "untrusted"
		if s.Trusted {
			trust = "trusted"
		}
		st := r.h.stats[s.Name]
		line := fmt.Sprintf("  - %s (%s): %d tools", s.Name, trust, len(s.Tools))
		if st != nil {
			calls += st.calls
			errs += st.errors
			line += fmt.Sprintf(", %d calls, %d errors", st.calls, st.errors)
			if !st.last.IsZero() {
				line += fmt.Sprintf(", last %s", st.last.Format(time.RFC3339))
			}
		}
		full.WriteString(line + "\n")
	}
	full.WriteString("Done: —\nWaiting: —\n")

	// The summary is read by a person, so it reports what was done, not what
	// is plugged in: server inventory belongs in status_report (D-053).
	summary := ""
	switch {
	case calls == 0:
		summary = ""
	case errs == 0:
		summary = fmt.Sprintf("I've made %d tool call%s since starting up", calls, plural(calls))
	default:
		summary = fmt.Sprintf("I've made %d tool call%s since starting up and %d failed", calls, plural(calls), errs)
	}
	return full.String(), summary
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
