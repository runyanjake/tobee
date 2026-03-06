package memory

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	chromem "github.com/philippgille/chromem-go"
)

// Store holds memories in two layers:
//   - A key-value map for O(1) exact ID lookup and metadata
//   - A chromem-go vector collection for semantic (approximate) search
//
// The vector layer is optional: if embeddingFunc is nil the store falls back
// to a simple substring scan on Store.Search.
type Store struct {
	mu         sync.RWMutex
	byID       map[string]*Memory
	collection *chromem.Collection // nil when no embedding function provided
}

// NewStore creates a Store. embeddingFunc may be nil to disable vector search.
func NewStore(ctx context.Context, embeddingFunc chromem.EmbeddingFunc) (*Store, error) {
	s := &Store{byID: make(map[string]*Memory)}

	if embeddingFunc != nil {
		db := chromem.NewDB()
		col, err := db.CreateCollection("memories", nil, embeddingFunc)
		if err != nil {
			return nil, fmt.Errorf("creating vector collection: %w", err)
		}
		s.collection = col
		log.Println("memory: vector store ready")
	} else {
		log.Println("memory: vector store disabled (no embedding function); using keyword fallback")
	}

	return s, nil
}

// Save stores a memory. If the memory's ID already exists it is overwritten.
func (s *Store) Save(ctx context.Context, m *Memory) error {
	if m.ID == "" {
		return fmt.Errorf("memory ID must not be empty")
	}
	m.Timestamp = time.Now()
	if m.LastAccessedAt.IsZero() {
		m.LastAccessedAt = m.Timestamp
	}

	s.mu.Lock()
	s.byID[m.ID] = m
	s.mu.Unlock()

	if s.collection != nil {
		meta := map[string]string{
			"source_id":  m.SourceID,
			"status":     string(m.Status),
			"tags":       strings.Join(m.Tags, ","),
			"importance": fmt.Sprintf("%.2f", m.Importance),
		}
		if err := s.collection.AddDocument(ctx, chromem.Document{
			ID:       m.ID,
			Metadata: meta,
			Content:  m.Content,
		}); err != nil {
			return fmt.Errorf("adding to vector store: %w", err)
		}
	}

	return nil
}

// Get retrieves a memory by ID and updates its last-accessed timestamp.
func (s *Store) Get(id string) (*Memory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if ok {
		m.LastAccessedAt = time.Now()
	}
	return m, ok
}

// Search returns up to limit active memories most relevant to query.
// Uses vector similarity when available, otherwise falls back to substring scan.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]*Memory, error) {
	if s.collection != nil {
		return s.vectorSearch(ctx, query, limit)
	}
	return s.keywordSearch(query, limit), nil
}

func (s *Store) vectorSearch(ctx context.Context, query string, limit int) ([]*Memory, error) {
	results, err := s.collection.Query(ctx, query, limit, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Memory
	for _, r := range results {
		m, ok := s.byID[r.ID]
		if ok && m.Status == StatusActive {
			m.LastAccessedAt = time.Now()
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) keywordSearch(query string, limit int) []*Memory {
	lower := strings.ToLower(query)
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*Memory
	for _, m := range s.byID {
		if m.Status != StatusActive {
			continue
		}
		if strings.Contains(strings.ToLower(m.Content), lower) {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// List returns all active memories, optionally filtered by tag, sorted by importance descending.
// Pass an empty tag to return all active memories.
func (s *Store) List(tag string) []*Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*Memory
	for _, m := range s.byID {
		if m.Status != StatusActive {
			continue
		}
		if tag != "" {
			found := false
			for _, t := range m.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Importance > out[j].Importance
	})
	return out
}

// SearchStrings returns up to limit active memories relevant to query as
// formatted "[id] content" strings. Satisfies the agent.MemorySearcher interface.
func (s *Store) SearchStrings(ctx context.Context, query string, limit int) ([]string, error) {
	results, err := s.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(results))
	for i, m := range results {
		out[i] = fmt.Sprintf("[%s] %s", m.ID, m.Content)
	}
	return out, nil
}

// Archive marks a memory as archived (soft delete).
func (s *Store) Archive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if ok {
		m.Status = StatusArchived
	}
	return ok
}
