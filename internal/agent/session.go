package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"tobee/internal/llm"
)

// Session is the short-term chat history for one (integration, channel, thread)
// scope. It keeps the last N turns verbatim in memory and persists them to a
// rolling `current.md` summary file managed by the summarizer.
type Session struct {
	Key      string
	recent   []llm.Message
	maxTurns int
}

// NewSession creates an empty session. A "turn" is one user→assistant pair,
// so maxTurns*2 is roughly the cap on retained messages.
func NewSession(key string, maxTurns int) *Session {
	if maxTurns <= 0 {
		maxTurns = 10
	}
	return &Session{Key: key, maxTurns: maxTurns}
}

// Append records an LLM message (user, assistant, or tool) and trims the
// buffer so it never holds more than maxTurns*2 entries.
func (s *Session) Append(m llm.Message) {
	s.recent = append(s.recent, m)
	cap := s.maxTurns * 2
	if len(s.recent) > cap {
		s.recent = s.recent[len(s.recent)-cap:]
	}
}

// Recent returns a copy of the in-memory transcript tail.
func (s *Session) Recent() []llm.Message {
	out := make([]llm.Message, len(s.recent))
	copy(out, s.recent)
	return out
}

// SessionStore is an in-memory registry of Sessions, keyed by Envelope.Key().
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	maxTurns int
	rootDir  string // data/sessions
}

func NewSessionStore(rootDir string, maxTurns int) (*SessionStore, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir sessions: %w", err)
	}
	return &SessionStore{
		sessions: make(map[string]*Session),
		maxTurns: maxTurns,
		rootDir:  rootDir,
	}, nil
}

// Get returns the Session for key, creating one on first access.
func (s *SessionStore) Get(key string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[key]
	if !ok {
		sess = NewSession(key, s.maxTurns)
		s.sessions[key] = sess
	}
	return sess
}

// SummaryPath returns the absolute path to the rolling summary file for
// a session key. The path encodes integration/channel[/thread] as directories
// so humans can browse `data/sessions/discord/<channel>/current.md`.
func (s *SessionStore) SummaryPath(key string) string {
	rel := relFromKey(key)
	return filepath.Join(s.rootDir, rel, "current.md")
}

// ReadSummary returns the stored rolling summary, or "" if none exists yet.
func (s *SessionStore) ReadSummary(key string) string {
	path := s.SummaryPath(key)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		return ""
	}
	return string(data)
}

// WriteSummary replaces the rolling summary file.
func (s *SessionStore) WriteSummary(key, content string) error {
	path := s.SummaryPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// relFromKey converts "integration:channel[:thread]" into a forward-slash path.
func relFromKey(key string) string {
	// Keep this cheap; the key is already safe (produced by Envelope.Key()).
	out := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			out = append(out, '/')
		} else {
			out = append(out, key[i])
		}
	}
	return string(out)
}
