package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ActionFunc is the signature for all registered actions.
type ActionFunc func(ctx context.Context, s *State, args map[string]string) (string, error)

// State is the shared application state passed between packages.
type State struct {
	mu                sync.RWMutex
	Context           map[string]string // context/<filename> -> file contents
	AdditionalContext map[string]string // runtime key -> value
	Actions           map[string]ActionFunc
}

func New() *State {
	return &State{
		Context:           make(map[string]string),
		AdditionalContext: make(map[string]string),
		Actions:           make(map[string]ActionFunc),
	}
}

// LoadContextDir reads all files in dir and populates State.Context.
func (s *State) LoadContextDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		s.Context[entry.Name()] = string(data)
	}
	return nil
}

func (s *State) RegisterAction(name string, fn ActionFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Actions[name] = fn
}

func (s *State) GetAction(name string) (ActionFunc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn, ok := s.Actions[name]
	return fn, ok
}

func (s *State) ListActions() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.Actions))
	for name := range s.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *State) SetAdditionalContext(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AdditionalContext[key] = value
}

func (s *State) DeleteAdditionalContext(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.AdditionalContext, key)
}

func (s *State) GetAdditionalContext(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.AdditionalContext[key]
	return v, ok
}

// BuildSystemPrompt concatenates all context files (sorted by name) followed
// by any runtime additional context into a single system prompt string.
func (s *State) BuildSystemPrompt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sb strings.Builder

	keys := make([]string, 0, len(s.Context))
	for k := range s.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(s.Context[k])
		sb.WriteString("\n\n")
	}

	addKeys := make([]string, 0, len(s.AdditionalContext))
	for k := range s.AdditionalContext {
		addKeys = append(addKeys, k)
	}
	sort.Strings(addKeys)
	for _, k := range addKeys {
		sb.WriteString(s.AdditionalContext[k])
		sb.WriteString("\n\n")
	}

	return strings.TrimSpace(sb.String())
}

