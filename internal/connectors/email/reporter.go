package email

import (
	"context"
	"fmt"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes mailbox activity via abilities.Reporter.
func (m *Mailbox) Reporter() abilities.Reporter { return mailReporter{m: m} }

type mailReporter struct{ m *Mailbox }

func (r mailReporter) Name() string { return Name }

// Render reports lifetime counters; `since` does not narrow them.
func (r mailReporter) Render(_ context.Context, _ time.Time) (string, string) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	doing := fmt.Sprintf("polling %s every %s", r.m.cfg.Mailbox, r.m.cfg.Interval)
	if !r.m.lastPoll.IsZero() {
		doing += fmt.Sprintf(", last poll %s", r.m.lastPoll.UTC().Format(time.RFC3339))
	}
	if r.m.lastErr != "" {
		doing += fmt.Sprintf(", last error: %s", r.m.lastErr)
	}
	full := fmt.Sprintf("Doing: %s\nDone: %d received, %d sent since boot\nWaiting: —\n", doing, r.m.received, r.m.sent)

	summary := ""
	if r.m.received > 0 || r.m.sent > 0 {
		summary = fmt.Sprintf("Email has received %d and sent %d message%s since boot", r.m.received, r.m.sent, plural(r.m.sent))
	}
	return full, summary
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
