package core

// Queue is a buffered channel of inbound Messages from any integration.
type Queue struct {
	ch chan Message
}

// NewQueue creates a Queue with the given buffer size.
func NewQueue(size int) *Queue {
	return &Queue{ch: make(chan Message, size)}
}

// Push enqueues a message. Blocks if the buffer is full.
func (q *Queue) Push(msg Message) {
	q.ch <- msg
}

// C returns the receive-only channel, used by the core loop to drain messages.
func (q *Queue) C() <-chan Message {
	return q.ch
}
