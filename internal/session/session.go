// Package session keeps each person's recent conversation, across
// connectors, until it goes idle (D-046). A session holds only what
// happened — the person's messages, the tool calls made and their real
// results, and the replies delivered — never the model's private chatter.
// Idle sessions are handed to an archive function and dropped.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
)

// Caps keep history a bounded slice of the prompt.
const (
	maxToolResultBytes = 1000
	maxHistoryBytes    = 16 * 1024
)

// Exchange is one turn: the person's message, everything tobee did, and how
// the turn ended.
type Exchange struct {
	At        time.Time     `json:"at"`
	Connector string        `json:"connector"`
	Channel   string        `json:"channel"`
	UserName  string        `json:"userName,omitempty"`
	Messages  []llm.Message `json:"messages"`
	Outcome   *Outcome      `json:"outcome,omitempty"`
}

// Outcome is what was tried and what came of it, written by code from the
// turn's own record (D-051). Kept only when something is worth carrying: a
// clean turn's messages already say everything.
type Outcome struct {
	Status   string   `json:"status"` // replied | no reply | parked
	Steps    int      `json:"steps"`
	Acted    []string `json:"acted,omitempty"` // "✅ memory_write: wrote …"
	Problems []string `json:"problems,omitempty"`
}

// Line renders the outcome for the model: one tagged message, so a later turn
// knows what was already tried and what failed rather than repeating it.
func (o *Outcome) Line() string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<outcome status=%q steps=%d>", o.Status, o.Steps)
	for _, a := range o.Acted {
		fmt.Fprintf(&b, "\n%s", a)
	}
	for _, p := range o.Problems {
		fmt.Fprintf(&b, "\nfailed: %s", p)
	}
	b.WriteString("\n</outcome>")
	return b.String()
}

// Notable reports whether this outcome is worth showing a later turn.
func (o *Outcome) Notable() bool {
	return o != nil && (len(o.Problems) > 0 || o.Status != "replied")
}

type Session struct {
	Person     string     `json:"person"`
	Started    time.Time  `json:"started"`
	LastActive time.Time  `json:"lastActive"`
	Exchanges  []Exchange `json:"exchanges"`
}

// ArchiveFunc receives a session that went idle. An error keeps the
// session on disk so the next sweep retries.
type ArchiveFunc func(*Session) error

type Store struct {
	dir     string
	idle    time.Duration
	archive ArchiveFunc

	mu       sync.Mutex
	sessions map[string]*Session // person → session
}

// Open loads sessions saved under dir.
func Open(dir string, idle time.Duration, archive ArchiveFunc) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: mkdir: %w", err)
	}
	s := &Store{dir: dir, idle: idle, archive: archive, sessions: map[string]*Session{}}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		var sess Session
		if err == nil {
			err = json.Unmarshal(b, &sess)
		}
		if err != nil || sess.Person == "" {
			slog.Warn("session: unreadable; skipping", "file", f, "err", err)
			continue
		}
		s.sessions[sess.Person] = &sess
	}
	return s, nil
}

// History returns the person's live session as chat messages, newest
// exchanges kept whole until maxHistoryBytes. An idle session is archived
// first, so a stale conversation never leaks into a new one.
func (s *Store) History(person string, now time.Time) []llm.Message {
	if person == "" {
		return nil
	}
	s.expire(now, person)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[person]
	if !ok {
		return nil
	}
	var keep [][]llm.Message
	size := 0
	for i := len(sess.Exchanges) - 1; i >= 0; i-- {
		n := 0
		for _, m := range sess.Exchanges[i].Messages {
			n += len(m.Content)
			for _, tc := range m.ToolCalls {
				n += len(tc.Function.Arguments)
			}
		}
		if size+n > maxHistoryBytes && len(keep) > 0 {
			break
		}
		size += n
		msgs := sess.Exchanges[i].Messages
		// A failed turn carries its outcome into the next one: without it the
		// model repeats what didn't work (D-051).
		if o := sess.Exchanges[i].Outcome; o.Notable() {
			msgs = append(append([]llm.Message{}, msgs...),
				llm.Message{Role: llm.RoleUser, Content: o.Line()})
		}
		keep = append(keep, msgs)
	}
	var out []llm.Message
	for i := len(keep) - 1; i >= 0; i-- {
		out = append(out, keep[i]...)
	}
	return out
}

// Record appends an exchange to the person's session, starting one if needed.
func (s *Store) Record(person string, ex Exchange) {
	if person == "" || len(ex.Messages) == 0 {
		return
	}
	for i, m := range ex.Messages {
		if m.Role == llm.RoleTool && len(m.Content) > maxToolResultBytes {
			ex.Messages[i].Content = m.Content[:maxToolResultBytes] + "…[truncated]"
		}
	}
	s.mu.Lock()
	sess, ok := s.sessions[person]
	if !ok {
		sess = &Session{Person: person, Started: ex.At}
		s.sessions[person] = sess
	}
	sess.LastActive = ex.At
	sess.Exchanges = append(sess.Exchanges, ex)
	b, err := json.MarshalIndent(sess, "", "  ")
	s.mu.Unlock()
	if err == nil {
		err = writeAtomic(s.path(person), b)
	}
	if err != nil {
		slog.Warn("session: save failed; history is in memory only", "person", person, "err", err)
	}
}

// Run archives idle sessions every interval until ctx is done.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	s.expire(time.Now(), "")
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.expire(now, "")
		}
	}
}

// expire archives idle sessions: all of them, or only person's when set.
func (s *Store) expire(now time.Time, person string) {
	s.mu.Lock()
	var idle []*Session
	for p, sess := range s.sessions {
		if (person == "" || p == person) && now.Sub(sess.LastActive) >= s.idle {
			idle = append(idle, sess)
			delete(s.sessions, p)
		}
	}
	s.mu.Unlock()

	for _, sess := range idle {
		if s.archive != nil {
			if err := s.archive(sess); err != nil {
				slog.Error("session: archive failed; will retry", "person", sess.Person, "err", err)
				s.mu.Lock()
				if _, fresh := s.sessions[sess.Person]; !fresh {
					s.sessions[sess.Person] = sess
				}
				s.mu.Unlock()
				continue
			}
		}
		if err := os.Remove(s.path(sess.Person)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("session: remove failed", "person", sess.Person, "err", err)
		}
		slog.Info("session: closed", "person", sess.Person, "exchanges", len(sess.Exchanges),
			"started", sess.Started.Format(time.RFC3339))
	}
}

func (s *Store) path(person string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, person)
	return filepath.Join(s.dir, safe+".json")
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Markdown renders the session as a human-readable transcript: what was
// said and what was done, from the recorded messages only.
func (sess *Session) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Conversation %s – %s\n", sess.Started.Local().Format("2006-01-02 15:04"),
		sess.LastActive.Local().Format("15:04 MST"))
	connectors := map[string]bool{}
	for _, ex := range sess.Exchanges {
		connectors[ex.Connector] = true
	}
	names := make([]string, 0, len(connectors))
	for c := range connectors {
		names = append(names, c)
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "\nPerson: %s. Via: %s.\n", sess.Person, strings.Join(names, ", "))

	for _, ex := range sess.Exchanges {
		who := ex.UserName
		if who == "" {
			who = "user"
		}
		for _, m := range ex.Messages {
			switch {
			case m.Role == llm.RoleUser:
				fmt.Fprintf(&b, "\n**%s** (%s, %s): %s\n", who, ex.Connector, ex.At.Local().Format("15:04"), m.Content)
			case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "\n> called `%s` %s\n", tc.Function.Name, oneLine(tc.Function.Arguments))
				}
			case m.Role == llm.RoleAssistant:
				fmt.Fprintf(&b, "\n**tobee**: %s\n", m.Content)
			case m.Role == llm.RoleTool:
				fmt.Fprintf(&b, "> result: %s\n", oneLine(m.Content))
			}
		}
		// What went wrong is part of the record: a transcript that only shows
		// the reply teaches the wrong lesson when the reply was wrong (D-051).
		if o := ex.Outcome; o.Notable() {
			fmt.Fprintf(&b, "\n> outcome: %s after %d step%s\n", o.Status, o.Steps, plural(o.Steps))
			for _, p := range o.Problems {
				fmt.Fprintf(&b, "> failed: %s\n", p)
			}
		}
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
