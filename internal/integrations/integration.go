// Package integrations defines the contract every external adapter (Discord,
// Slack, webhook, scheduler, etc.) satisfies, plus the Envelope type that
// carries an inbound event from any source into the agent.
package integrations

import (
	"context"
	"time"
)

// Integration is an external adapter. Lifecycle is managed by main.
type Integration interface {
	// Name is a stable, lowercase identifier used in logs and envelopes.
	Name() string
	// Start connects to the external service and begins pushing Envelopes
	// onto its configured bus. It must return promptly.
	Start(ctx context.Context) error
	// Stop gracefully disconnects.
	Stop() error
}

// Envelope is a normalized inbound event delivered from any integration to
// the agent. Integrations own how these fields map to their transport.
type Envelope struct {
	Integration string // "discord", "slack", "scheduler", ...
	User        string // stable per-integration user id, "" for system events
	Channel     string // opaque routing id used when replying
	Thread      string // optional thread id within a channel
	Content     string // user-facing text body
	Received    time.Time
}

// Key returns a stable string that identifies the session scope for history
// and summarisation: one conversation thread per (integration, channel, thread).
func (e Envelope) Key() string {
	if e.Thread != "" {
		return e.Integration + ":" + e.Channel + ":" + e.Thread
	}
	return e.Integration + ":" + e.Channel
}
