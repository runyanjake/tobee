package memory

import "time"

// Status represents the lifecycle state of a memory.
type Status string

const (
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

// Memory is a single piece of stored knowledge.
type Memory struct {
	ID             string
	Content        string
	Timestamp      time.Time
	Importance     float32  // 0.0 (trivial) – 1.0 (critical)
	LastAccessedAt time.Time
	Tags           []string // thematic labels, e.g. ["user_preference", "discord"]
	SourceID       string   // integration that produced this memory, e.g. "discord"
	Status         Status
}
