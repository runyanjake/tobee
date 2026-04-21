package integrations

import "log/slog"

// Bus is a buffered channel of inbound Envelopes from any integration.
// Producers call Publish; the single agent consumer receives via C.
type Bus struct {
	ch chan Envelope
}

// NewBus creates a Bus with the given buffer size.
func NewBus(size int) *Bus {
	if size <= 0 {
		size = 64
	}
	return &Bus{ch: make(chan Envelope, size)}
}

// Publish enqueues e. If the buffer is full the event is dropped with a
// warning — the agent is persistently behind, and blocking the producer
// (e.g. a Discord gateway goroutine) would be worse than a dropped event.
func (b *Bus) Publish(e Envelope) {
	select {
	case b.ch <- e:
	default:
		slog.Warn("bus: buffer full, dropping envelope",
			"integration", e.Integration, "channel", e.Channel)
	}
}

// C is the receive-only channel drained by the agent worker.
func (b *Bus) C() <-chan Envelope { return b.ch }
