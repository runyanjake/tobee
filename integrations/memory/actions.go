package memory

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"tobee/internal/agent"
	"tobee/internal/msg"
)

// Register wires memory actions onto s using the given Store.
func Register(s *agent.State, store *Store) {
	s.RegisterAction("memory.store", storeAction(store))
	s.RegisterAction("memory.recall", recallAction(store))
	s.RegisterAction("memory.forget", forgetAction(store))
	s.RegisterAction("memory.list", listAction(store))
}

// storeAction saves a new memory.
// Required args: content
// Optional args: importance (0.0–1.0, default 0.5), tags (comma-separated), id
func storeAction(store *Store) agent.ActionFunc {
	return func(ctx context.Context, _ *agent.State, args map[string]string) (string, error) {
		content := args["content"]
		if content == "" {
			return "", fmt.Errorf("memory.store requires 'content' arg")
		}

		id := args["id"]
		if id == "" {
			id = fmt.Sprintf("mem_%d", uniqueID())
		}

		importance := float32(0.5)
		if v := args["importance"]; v != "" {
			var f float64
			fmt.Sscanf(v, "%f", &f)
			importance = float32(f)
		}

		var tags []string
		if t := args["tags"]; t != "" {
			for _, tag := range strings.Split(t, ",") {
				if trimmed := strings.TrimSpace(tag); trimmed != "" {
					tags = append(tags, trimmed)
				}
			}
		}

		sourceID := "system"
		if m, ok := msg.MessageFrom(ctx); ok {
			sourceID = m.Integration
		}

		m := &Memory{
			ID:         id,
			Content:    content,
			Importance: importance,
			Tags:       tags,
			SourceID:   sourceID,
			Status:     StatusActive,
		}
		if err := store.Save(ctx, m); err != nil {
			return "", fmt.Errorf("saving memory: %w", err)
		}
		return fmt.Sprintf("memory stored: %s", id), nil
	}
}

// recallAction performs a semantic search across memories.
// Required args: query
// Optional args: limit (default 5)
func recallAction(store *Store) agent.ActionFunc {
	return func(ctx context.Context, _ *agent.State, args map[string]string) (string, error) {
		query := args["query"]
		if query == "" {
			return "", fmt.Errorf("memory.recall requires 'query' arg")
		}

		limit := 5
		if v := args["limit"]; v != "" {
			fmt.Sscanf(v, "%d", &limit)
		}

		results, err := store.Search(ctx, query, limit)
		if err != nil {
			return "", fmt.Errorf("searching memories: %w", err)
		}
		if len(results) == 0 {
			return "no memories found", nil
		}

		var sb strings.Builder
		for _, m := range results {
			fmt.Fprintf(&sb, "[%s] %s\n", m.ID, m.Content)
		}
		return strings.TrimSpace(sb.String()), nil
	}
}

// forgetAction archives a memory by ID.
// Required args: id
func forgetAction(store *Store) agent.ActionFunc {
	return func(_ context.Context, _ *agent.State, args map[string]string) (string, error) {
		id := args["id"]
		if id == "" {
			return "", fmt.Errorf("memory.forget requires 'id' arg")
		}
		if !store.Archive(id) {
			return fmt.Sprintf("memory not found: %s", id), nil
		}
		return fmt.Sprintf("memory archived: %s", id), nil
	}
}

// listAction returns all active memories, optionally filtered by tag.
// Optional args: tag (filter by tag), limit (default 0 = all)
func listAction(store *Store) agent.ActionFunc {
	return func(_ context.Context, _ *agent.State, args map[string]string) (string, error) {
		tag := args["tag"]
		memories := store.List(tag)
		if len(memories) == 0 {
			return "no memories found", nil
		}

		limit := 0
		if v := args["limit"]; v != "" {
			fmt.Sscanf(v, "%d", &limit)
		}

		var sb strings.Builder
		for i, m := range memories {
			if limit > 0 && i >= limit {
				break
			}
			fmt.Fprintf(&sb, "[%s] (importance=%.1f) %s\n", m.ID, m.Importance, m.Content)
		}
		return strings.TrimSpace(sb.String()), nil
	}
}

var idCounter atomic.Uint64

func uniqueID() uint64 {
	return idCounter.Add(1)
}
