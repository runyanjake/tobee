// Package event defines the normalized inbound event every ingest source
// produces and the address a reply to it is delivered to. It sits below
// ingest, the task queue, delivery, and the agent so none of them import
// each other for these types.
package event

import "time"

// Kind classifies what produced an Event.
type Kind string

const (
	KindMessage      Kind = "message"      // a person wrote to tobee
	KindTimer        Kind = "timer"        // a scheduled job fired
	KindNotification Kind = "notification" // an external system reported a change
)

// Address is where output for an event is delivered. Connector names the
// delivery channel ("discord", "email"); Channel and Thread are opaque to
// everything but that connector.
type Address struct {
	Connector string `json:"connector"`
	Channel   string `json:"channel"`
	Thread    string `json:"thread,omitempty"`
}

// Actor is who caused an event. ID is stable per connector; an empty ID
// means no person is attached (a timer, a resource notification).
type Actor struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Event is one normalized inbound event.
type Event struct {
	// ID is unique per Source and is the dedup key: a poller that sees the
	// same message twice must produce the same ID both times.
	ID     string `json:"id"`
	Source string `json:"source"`
	Kind   Kind   `json:"kind"`
	Actor  Actor  `json:"actor"`

	// Origin is where the reply goes.
	Origin Address `json:"origin"`
	// MessageID is the connector's id for the inbound message, used to
	// react to it. Empty when there is nothing to react to.
	MessageID string `json:"messageId,omitempty"`
	// InReplyTo is the connector's id for the message this one answers
	// (a Discord reply, an email In-Reply-To). Used to resume a parked task.
	InReplyTo string `json:"inReplyTo,omitempty"`

	Content  string    `json:"content"`
	Received time.Time `json:"received"`
}

// ReplyKey identifies "a reply to message msgID at addr". A parked task
// registers it for the question it sent; an event whose InReplyTo names
// that message produces the same key (D-036).
func ReplyKey(addr Address, msgID string) string {
	return "reply:" + addr.Connector + ":" + addr.Channel + ":" + msgID
}

// ActorKey identifies "the next message from actorID at addr". The
// fallback match when the answer is not an explicit reply.
func ActorKey(addr Address, actorID string) string {
	return "actor:" + addr.Connector + ":" + addr.Channel + ":" + actorID
}

// ResumeKeys returns the keys e can resume a parked task with, most
// specific first. Only messages from a person can answer a question.
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
