package integration

import "context"

// Integration is implemented by any external adapter (Discord, Slack, webhooks, etc.).
// Each integration is responsible for connecting to its service, routing inbound
// messages to the core, and sending outbound responses.
type Integration interface {
	// Name returns a human-readable identifier used in logs.
	Name() string
	// Start connects to the external service and begins handling messages.
	// It must return promptly; long-running work belongs in goroutines.
	Start(ctx context.Context) error
	// Stop gracefully disconnects from the external service.
	Stop() error
}
