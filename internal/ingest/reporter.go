package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

func (e *Engine) Reporter() abilities.Reporter { return engineReporter{e: e} }

type engineReporter struct{ e *Engine }

func (r engineReporter) Name() string { return "ingest" }

// Render ignores `since`: the engine keeps no per-event history.
func (r engineReporter) Render(_ context.Context, _ time.Time) (string, string) {
	r.e.mu.Lock()
	runs := make([]*running, 0, len(r.e.sources))
	for _, s := range r.e.sources {
		runs = append(runs, s)
	}
	r.e.mu.Unlock()
	sort.Slice(runs, func(i, j int) bool { return runs[i].src.Name() < runs[j].src.Name() })

	var full strings.Builder
	full.WriteString("Doing:\n")
	down := 0
	for _, s := range runs {
		s.mu.Lock()
		state := "up"
		if !s.up {
			state = "down"
			down++
		}
		line := fmt.Sprintf("  - %s: %s, %d admitted, %d dropped, %d restarts",
			s.src.Name(), state, s.admitted, s.dropped, s.restarts)
		if !s.lastAt.IsZero() {
			line += fmt.Sprintf(", last event %s", s.lastAt.UTC().Format(time.RFC3339))
		}
		if s.lastErr != "" {
			line += fmt.Sprintf(", last error: %s", s.lastErr)
		}
		s.mu.Unlock()
		full.WriteString(line + "\n")
	}
	full.WriteString("Done: —\nWaiting: —\n")

	summary := fmt.Sprintf("%d input source%s running", len(runs)-down, plural(len(runs)-down))
	if down > 0 {
		summary += fmt.Sprintf(", %d down", down)
	}
	return full.String(), summary
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
