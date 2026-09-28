// Package abilities holds the Reporter contract behind the status tools.
// Output is appended to replies verbatim (D-030), so it must be deterministic.
package abilities

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reporter's Render returns full (status_report) and one-sentence summary (status_summary)
// text, shown verbatim; return "" when there is nothing to say. Name must be unique.
type Reporter interface {
	Name() string
	Render(ctx context.Context, since time.Time) (full, summary string)
}

type Registry struct {
	mu   sync.RWMutex
	reps map[string]Reporter
}

func NewRegistry() *Registry {
	return &Registry{reps: make(map[string]Reporter)}
}

// Register replaces any earlier Reporter with the same name.
func (r *Registry) Register(rep Reporter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reps[rep.Name()] = rep
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.reps))
	for n := range r.reps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RenderReport shows silent reporters as "(idle)" so the user sees they were asked.
func (r *Registry) RenderReport(ctx context.Context, since time.Time) string {
	now := time.Now().UTC()
	var b strings.Builder
	fmt.Fprintf(&b, "tobee status — window %s → %s\n",
		since.UTC().Format(time.RFC3339), now.Format(time.RFC3339))
	for _, rep := range r.sorted() {
		full, _ := rep.Render(ctx, since)
		full = strings.TrimSpace(full)
		fmt.Fprintf(&b, "\n## %s\n", rep.Name())
		if full == "" {
			b.WriteString("(idle)\n")
			continue
		}
		b.WriteString(full)
		if !strings.HasSuffix(full, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// RenderSummary drops empty summaries and falls back to "Everything quiet.".
func (r *Registry) RenderSummary(ctx context.Context, since time.Time) string {
	var parts []string
	for _, rep := range r.sorted() {
		_, summary := rep.Render(ctx, since)
		summary = strings.TrimSpace(summary)
		if summary == "" {
			continue
		}
		if !strings.HasSuffix(summary, ".") {
			summary += "."
		}
		parts = append(parts, summary)
	}
	if len(parts) == 0 {
		return "Everything quiet."
	}
	return strings.Join(parts, " ")
}

func (r *Registry) sorted() []Reporter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.reps))
	for n := range r.reps {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Reporter, 0, len(names))
	for _, n := range names {
		out = append(out, r.reps[n])
	}
	return out
}
