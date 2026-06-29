package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
)

// recentFileName is the on-disk transcript persisted alongside current.md.
// It captures the exact ring buffer so warm context survives a restart.
const recentFileName = "recent.json"

// Session is the short-term chat history for one (integration, channel, thread)
// scope. It keeps the last N turns verbatim in memory and mirrors them to
// `recent.json` on every Append so a restart resumes mid-conversation.
type Session struct {
	Key          string
	recent       []llm.Message
	maxTurns     int
	lastActivity time.Time

	isDirect    bool   // chooses which idle timeout applies
	persistPath string // recent.json path; "" disables persistence
}

// sessionState is the on-disk shape of recent.json. Kind is mirrored from
// Session.isDirect so a post-restart sweep can pick the right timeout
// without needing the originating envelope.
type sessionState struct {
	Kind     string        `json:"kind"`
	Messages []llm.Message `json:"messages"`
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
// buffer so it never holds more than maxTurns*2 entries. Persists the
// updated buffer to recent.json if persistPath is set.
func (s *Session) Append(m llm.Message) {
	s.recent = append(s.recent, m)
	cap := s.maxTurns * 2
	if len(s.recent) > cap {
		s.recent = s.recent[len(s.recent)-cap:]
	}
	s.lastActivity = time.Now()
	if s.persistPath != "" {
		if err := writeRecent(s.persistPath, s.kindString(), s.recent); err != nil {
			slog.Warn("session: persist failed", "key", s.Key, "err", err)
		}
	}
}

func (s *Session) kindString() string {
	if s.isDirect {
		return "dm"
	}
	return "channel"
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
	mu            sync.Mutex
	sessions      map[string]*Session
	maxTurns      int
	rootDir       string // data/sessions
	idleTimeoutCh time.Duration
	idleTimeoutDM time.Duration
}

// NewSessionStore builds a store rooted at rootDir. idleTimeoutCh applies to
// channel sessions; idleTimeoutDM applies to one-on-one sessions (e.g. Discord
// DMs). When either is exceeded the session is rotated to archive/ on the next
// Get or SweepIdle. Non-positive disables idle rotation for that kind.
func NewSessionStore(rootDir string, maxTurns int, idleTimeoutCh, idleTimeoutDM time.Duration) (*SessionStore, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir sessions: %w", err)
	}
	return &SessionStore{
		sessions:      make(map[string]*Session),
		maxTurns:      maxTurns,
		rootDir:       rootDir,
		idleTimeoutCh: idleTimeoutCh,
		idleTimeoutDM: idleTimeoutDM,
	}, nil
}

// Get returns the Session for key, creating one on first access. isDirect is
// the kind hint from the inbound envelope; it picks which idle timeout
// applies and is persisted so post-restart sweeps pick the same one. If the
// existing session (or its on-disk state) has been idle past the relevant
// timeout it is rotated and a fresh session is returned.
func (s *SessionStore) Get(key string, isDirect bool) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[key]; ok {
		// Update kind on every hit — the envelope is the source of truth.
		// (A channel can't flip to a DM in practice, but be defensive.)
		sess.isDirect = isDirect
		if s.isSessionIdle(sess) {
			s.rotateLocked(key)
		} else {
			return sess
		}
	} else if s.stateIdle(key, isDirect) {
		// No in-memory entry, but stale state on disk from a previous
		// process lifetime — rotate before we create a fresh session.
		s.rotateLocked(key)
	}

	sess := NewSession(key, s.maxTurns)
	sess.isDirect = isDirect
	sess.persistPath = s.recentPath(key)
	s.loadRecentLocked(sess)
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
// activity exceeds the relevant idle timeout. Returns the number rotated.
// Safe to call from a separate goroutine.
func (s *SessionStore) SweepIdle() int {
	if s.idleTimeoutCh <= 0 && s.idleTimeoutDM <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rotated := 0
	for key, sess := range s.sessions {
		if s.isSessionIdle(sess) {
			if s.rotateLocked(key) {
				rotated++
			}
		}
	}

	// Pick up state files for keys we have no in-memory entry for (e.g.
	// after a restart). We use recent.json when available because it
	// carries the kind hint; we fall back to current.md so legacy trees
	// (pre-persisted-ring) still rotate. Walk shallow — we know the layout.
	visited := make(map[string]bool, len(s.sessions))
	_ = filepath.WalkDir(s.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if name != recentFileName && name != "current.md" {
			return nil
		}
		key, ok := keyFromStatePath(s.rootDir, path)
		if !ok {
			return nil
		}
		if _, inMem := s.sessions[key]; inMem {
			return nil
		}
		if visited[key] {
			return nil
		}
		visited[key] = true
		if s.diskIdleLocked(key) {
			if s.rotateLocked(key) {
				rotated++
			}
		}
		return nil
	})
	return rotated
}

// idleTimeoutFor returns the configured timeout for the given session kind.
func (s *SessionStore) idleTimeoutFor(isDirect bool) time.Duration {
	if isDirect {
		return s.idleTimeoutDM
	}
	return s.idleTimeoutCh
}

// isSessionIdle reports whether the in-memory session has exceeded the
// relevant idle threshold. Caller must hold s.mu.
func (s *SessionStore) isSessionIdle(sess *Session) bool {
	timeout := s.idleTimeoutFor(sess.isDirect)
	if timeout <= 0 {
		return false
	}
	return time.Since(sess.lastActivity) > timeout
}

// stateIdle checks whether the on-disk state for key is past the idle
// threshold for sessions of the given kind. Used in Get when no in-memory
// entry exists yet. Caller must hold s.mu.
func (s *SessionStore) stateIdle(key string, isDirect bool) bool {
	timeout := s.idleTimeoutFor(isDirect)
	if timeout <= 0 {
		return false
	}
	mtime, ok := s.latestStateMtime(key)
	if !ok {
		return false
	}
	return time.Since(mtime) > timeout
}

// diskIdleLocked is like stateIdle but discovers the kind from disk. Used
// during SweepIdle for keys we have no in-memory entry for. Caller must
// hold s.mu.
func (s *SessionStore) diskIdleLocked(key string) bool {
	mtime, ok := s.latestStateMtime(key)
	if !ok {
		return false
	}
	isDirect := s.diskIsDirect(key)
	timeout := s.idleTimeoutFor(isDirect)
	if timeout <= 0 {
		return false
	}
	return time.Since(mtime) > timeout
}

// latestStateMtime returns the most recent mtime of recent.json and
// current.md for this key; whichever exists wins, the newer of the two
// when both exist.
func (s *SessionStore) latestStateMtime(key string) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, p := range []string{s.recentPath(key), s.SummaryPath(key)} {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !found || info.ModTime().After(newest) {
			newest = info.ModTime()
			found = true
		}
	}
	return newest, found
}

// diskIsDirect reads kind from recent.json if present. Returns false (i.e.
// channel) when no kind hint is available — the more aggressive timeout.
func (s *SessionStore) diskIsDirect(key string) bool {
	data, err := os.ReadFile(s.recentPath(key))
	if err != nil {
		return false
	}
	var st sessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return false
	}
	return st.Kind == "dm"
}

// rotateLocked moves current.md to archive/<timestamp>.md (if it exists),
// removes recent.json, and drops the in-memory entry. Caller must hold s.mu.
// Returns true if something was rotated.
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
	// recent.json is exact transcript, not summary — there's nothing
	// useful to keep around once the session is reset.
	if err := os.Remove(s.recentPath(key)); err == nil {
		moved = true
	}
	if _, ok := s.sessions[key]; ok {
		delete(s.sessions, key)
		moved = true
	}
	if moved {
		slog.Debug("session: rotated", "key", key)
	}
	return moved
}

// recentPath returns the absolute path to recent.json for a session key.
func (s *SessionStore) recentPath(key string) string {
	rel := relFromKey(key)
	return filepath.Join(s.rootDir, rel, recentFileName)
}

// loadRecentLocked tries to populate sess.recent from recent.json. A
// missing or unreadable file is treated as "no prior state" — the session
// starts empty. Caller must hold s.mu.
func (s *SessionStore) loadRecentLocked(sess *Session) {
	data, err := os.ReadFile(s.recentPath(sess.Key))
	if err != nil {
		return
	}
	var st sessionState
	if err := json.Unmarshal(data, &st); err != nil {
		slog.Warn("session: recent.json unparseable; ignoring",
			"key", sess.Key, "err", err)
		return
	}
	sess.recent = st.Messages
	cap := sess.maxTurns * 2
	if len(sess.recent) > cap {
		sess.recent = sess.recent[len(sess.recent)-cap:]
	}
	if info, ierr := os.Stat(s.recentPath(sess.Key)); ierr == nil {
		sess.lastActivity = info.ModTime()
	}
}

// writeRecent atomically replaces recent.json. Caller is the Session
// itself; there is no mu held here. The tmp+rename ensures a crash mid-
// write cannot leave a partial file the next start would refuse to load.
func writeRecent(path, kind string, msgs []llm.Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	body, err := json.MarshalIndent(sessionState{Kind: kind, Messages: msgs}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "recent-*.json.tmp")
	if err != nil {
		return fmt.Errorf("tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
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

// keyFromStatePath derives a session key from the absolute path of a
// state file (recent.json or current.md) under rootDir. Returns ("", false)
// for paths outside rootDir or with too few path components.
func keyFromStatePath(rootDir, path string) (string, bool) {
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
