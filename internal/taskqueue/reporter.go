package taskqueue

import (
	"context"
	"fmt"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

func (q *Queue) Reporter() abilities.Reporter { return queueReporter{q: q} }

type queueReporter struct{ q *Queue }

func (r queueReporter) Name() string { return "tasks" }

func (r queueReporter) Render(_ context.Context, _ time.Time) (string, string) {
	pending, parked := r.q.Stats()
	full := fmt.Sprintf("Doing: —\nDone: —\nWaiting: %d queued, %d waiting on a user's answer\n", pending, parked)
	var summary string
	switch {
	case pending == 0 && parked == 0:
		summary = ""
	case parked == 0:
		summary = fmt.Sprintf("I have %d request%s still queued", pending, plural(pending))
	case pending == 0:
		summary = fmt.Sprintf("I'm waiting on your answer to %d question%s", parked, plural(parked))
	default:
		summary = fmt.Sprintf("I have %d request%s queued and I'm waiting on your answer to %d",
			pending, plural(pending), parked)
	}
	return full, summary
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
