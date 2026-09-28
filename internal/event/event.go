// Package event defines the normalized inbound event and reply address; it sits below
// ingest, the task queue, delivery, and the agent so they needn't import each other.
package event

import "time"

type Kind string

const (
	KindMessage      Kind = "message"      // a person wrote to tobee
	KindTimer        Kind = "timer"        // a scheduled job fired
	KindNotification Kind = "notification" // an external system reported a change
)

// Channel and Thread are opaque to everything but the named connector.
type Address struct {
	Connector string `json:"connector"`
	Channel   string `json:"channel"`
	Thread    string `json:"thread,omitempty"`
}

// Actor ID is stable per connector; empty means no person (a timer, a notification).
type Actor struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type Event struct {
	// ID is the dedup key: a poller seeing the same message twice must produce the same ID.
	ID     string `json:"id"`
	Source string `json:"source"`
	Kind   Kind   `json:"kind"`
	Actor  Actor  `json:"actor"`

	Origin Address `json:"origin"`
	// MessageID is the connector's id for the inbound message, used to react to it.
	MessageID string `json:"messageId,omitempty"`
	// InReplyTo names the message this one answers; it resumes a parked task.
	InReplyTo string `json:"inReplyTo,omitempty"`

	Content  string    `json:"content"`
	Received time.Time `json:"received"`
}

// ReplyKey matches a parked task's question to an event whose InReplyTo names it (D-036).
func ReplyKey(addr Address, msgID string) string {
	return "reply:" + addr.Connector + ":" + addr.Channel + ":" + msgID
}

// ActorKey is the fallback resume match when the answer is not an explicit reply.
func ActorKey(addr Address, actorID string) string {
	return "actor:" + addr.Connector + ":" + addr.Channel + ":" + actorID
}

// ResumeKeys returns most specific first; only a person's message can answer a question.
func (e Event) ResumeKeys() []string {
	if e.Kind != KindMessage || e.Actor.ID == "" {
		return nil
	}
	var keys []string
	if e.InReplyTo != "" {
		keys = append(keys, ReplyKey(e.Origin, e.InReplyTo))
	}
	return append(keys, ActorKey(e.Origin, e.Actor.ID))
}
