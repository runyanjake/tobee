package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tobee/internal/llm"
)

// Session is the short-term chat history for one (integration, channel, thread)
// scope. It keeps the last N turns verbatim in memory and persists them to a
// rolling `current.md` summary file managed by the summarizer.
type Session struct {
	Key          string
	recent       []llm.Message
	maxTurns     int
	lastActivity time.Time
}

// NewSession creates an empty session. A "turn" is one user→assistant pair,
// so maxTurns*2 is roughly the cap on retained messages.
func NewSession(key string, maxTurns int) *Session {
	if maxTurns <= 0 {
		maxTurns = 10
	}
	return &Session{Key: key, maxTurns: maxTurns, lastActivity: time.Now()}
}

// Append records an LLM message (user, assistant, or tool) and trims the
// buffer so it never holds more than maxTurns*2 entries.
func (s *Session) Append(m llm.Message) {
	s.recent = append(s.recent, m)
	cap := s.maxTurns * 2
	if len(s.recent) > cap {
		s.recent = s.recent[len(s.recent)-cap:]
	}
	s.lastActivity = time.Now()
}

// Recent returns a copy of the in-memory transcript tail.
func (s *Session) Recent() []llm.Message {
	out := make([]llm.Message, len(s.recent))
	copy(out, s.recent)
	return out
}

// LastActivity returns when the session last saw a message.
func (s *Session) LastActivity() time.Time { return s.lastActivity }

// SessionStore is an in-memory registry of Sessions, keyed by Envelope.Key().
type SessionStore struct {
	mu          sync.Mutex
	sessions    map[string]*Session
	maxTurns    int
	rootDir     string // data/sessions
	idleTimeout time.Duration
}

// NewSessionStore builds a store rooted at rootDir. idleTimeout controls how
// long a session may sit unused before Get and SweepIdle rotate it to
// archive/. Non-positive disables both checks.
func NewSessionStore(rootDir string, maxTurns int, idleTimeout time.Duration) (*SessionStore, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir sessions: %w", err)
	}
	return &SessionStore{
		sessions:    make(map[string]*Session),
		maxTurns:    maxTurns,
		rootDir:     rootDir,
		idleTimeout: idleTimeout,
	}, nil
}

// Get returns the Session for key, creating one on first access. If the
// existing session (or its on-disk summary) has been idle past the store's
// idleTimeout, it is rotated to archive/ and a fresh session is returned.
func (s *SessionStore) Get(key string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[key]; ok {
		if s.isIdle(sess.lastActivity) {
			s.rotateLocked(key)
		} else {
			return sess
		}
	} else if s.summaryIdle(key) {
		// No in-memory entry, but a stale current.md from a previous
		// process lifetime — rotate before we create a fresh session.
		s.rotateLocked(key)
	}

	sess := NewSession(key, s.maxTurns)
	s.sessions[key] = sess
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

// SweepIdle rotates every session — in memory or on disk — whose last
// activity exceeds the store's idleTimeout. Returns the number rotated.
// Safe to call from a separate goroutine.
func (s *SessionStore) SweepIdle() int {
	if s.idleTimeout <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rotated := 0
	for key, sess := range s.sessions {
		if s.isIdle(sess.lastActivity) {
			if s.rotateLocked(key) {
				rotated++
			}
		}
	}

	// Pick up current.md files for keys we have no in-memory entry for
	// (e.g. after a restart). Walk shallow — we know the layout.
	_ = filepath.WalkDir(s.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Name() != "current.md" {
			return nil
		}
		key, ok := keyFromSummaryPath(s.rootDir, path)
		if !ok {
			return nil
		}
		if _, inMem := s.sessions[key]; inMem {
			return nil // already handled above
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if time.Since(info.ModTime()) > s.idleTimeout {
			if s.rotateLocked(key) {
				rotated++
			}
		}
		return nil
	})
	return rotated
}

// isIdle reports whether t is older than the configured idle threshold.
func (s *SessionStore) isIdle(t time.Time) bool {
	if s.idleTimeout <= 0 {
		return false
	}
	return time.Since(t) > s.idleTimeout
}

// summaryIdle checks the on-disk current.md mtime for keys with no
// in-memory entry. Caller must hold s.mu.
func (s *SessionStore) summaryIdle(key string) bool {
	if s.idleTimeout <= 0 {
		return false
	}
	info, err := os.Stat(s.SummaryPath(key))
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > s.idleTimeout
}

// rotateLocked moves current.md to archive/<timestamp>.md (if it exists)
// and removes the in-memory entry. Caller must hold s.mu. Returns true if
// something was rotated (file moved or in-memory entry removed).
func (s *SessionStore) rotateLocked(key string) bool {
	moved := false
	src := s.SummaryPath(key)
	if info, err := os.Stat(src); err == nil && !info.IsDir() {
		archiveDir := filepath.Join(filepath.Dir(src), "archive")
		if err := os.MkdirAll(archiveDir, 0o755); err == nil {
			name := time.Now().UTC().Format("20060102T150405Z") + ".md"
			dst := filepath.Join(archiveDir, name)
			// On the off chance two rotations land in the same second,
			// suffix with a counter rather than clobbering.
			for i := 1; ; i++ {
				if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
					break
				}
				dst = filepath.Join(archiveDir, fmt.Sprintf("%s-%d.md",
					strings.TrimSuffix(name, ".md"), i))
			}
			if err := os.Rename(src, dst); err == nil {
				moved = true
			}
		}
	}
	if _, ok := s.sessions[key]; ok {
		delete(s.sessions, key)
		moved = true
	}
	return moved
}

// relFromKey converts "integration:channel[:thread]" into a forward-slash path.
func relFromKey(key string) string {
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

// keyFromSummaryPath is the inverse of SummaryPath: it derives a session
// key from a current.md absolute path under rootDir. Returns ("", false)
// for paths outside rootDir or with too few path components.
func keyFromSummaryPath(rootDir, path string) (string, bool) {
	rel, err := filepath.Rel(rootDir, filepath.Dir(path))
	if err != nil {
		return "", false
	}
	if rel == "." || rel == "" {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return "", false
	}
	return strings.Join(parts, ":"), true
}
