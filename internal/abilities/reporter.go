// Package abilities holds cross-cutting plumbing for things tobee *does*
// outside the LLM turn — surfacing what subsystems are up to, scheduled
// work, recent background activity, and (later) higher-level capabilities
// composed from them.
//
// The first primitive here is the Reporter contract used by the status.*
// tools. Any subsystem (scheduler, janitor, integration, …) can register
// a Reporter; the status tools call Render and compose deterministic text
// — full detail for status.report, a brief overview for status.summary.
//
// Determinism is the load-bearing property: the LLM is told to relay the
// composed text verbatim, so wording variability belongs in the reporter,
// not the model.
package abilities

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reporter is implemented by anything that wants to surface state through
// the status ability. Name is used as the section key and must be unique
// within a Registry.
//
// Render returns two deterministic views over the subsystem's state:
//
//   - full:    multi-line strict format used by status.report. Should
//     include every relevant Doing / Done / Waiting fact for the
//     window. Return "" when the subsystem genuinely has nothing
//     to say.
//   - summary: one short sentence used by status.summary. Return "" when
//     the subsystem is uninteresting (idle, no recent activity)
//     so the composed summary stays tight.
//
// Both strings are emitted verbatim to the user; the same state must
// always yield the same text.
type Reporter interface {
	Name() string
	Render(ctx context.Context, since time.Time) (full, summary string)
}

// Registry holds the Reporters status.* tools aggregate over.
type Registry struct {
	mu   sync.RWMutex
	reps map[string]Reporter
}

func NewRegistry() *Registry {
	return &Registry{reps: make(map[string]Reporter)}
}

// Register adds a Reporter. Later registrations under the same name
// overwrite earlier ones — convenient for testing, harmless in main
// wiring where names are programmer-chosen.
func (r *Registry) Register(rep Reporter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reps[rep.Name()] = rep
}

// Names returns registered reporter names, sorted.
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

// RenderReport composes the strict full-detail status block. Sections
// are ordered by reporter name; a reporter with nothing to say still
// gets a header line and "(idle)" so the user sees it was asked.
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

// RenderSummary composes the brief few-sentence overview. Per-reporter
// summaries that come back empty are dropped so the joined text stays
// tight; if every reporter is quiet, a single "Everything quiet." is
// returned.
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
