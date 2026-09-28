// Package taskqueue is the durable FIFO from ingest to the agent (D-034), and
// holds parked tasks: a request and question, never a transcript (D-036).
package taskqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/runyanjake/tobee/internal/event"
)

const ParkTTL = 24 * time.Hour

// maxAttempts stops a turn that crashes the process from looping at boot.
const maxAttempts = 2

var ErrFull = errors.New("taskqueue: full")

type Task struct {
	ID       string      `json:"id"`
	Event    event.Event `json:"event"`
	Resume   *Resume     `json:"resume,omitempty"`
	Enqueued time.Time   `json:"enqueued"`
	Attempts int         `json:"attempts"`
}

type Resume struct {
	TaskID   string    `json:"taskId"`
	Request  string    `json:"request"`
	Question string    `json:"question"`
	Asked    time.Time `json:"asked"`
}

type parked struct {
	TaskID   string    `json:"taskId"`
	Request  string    `json:"request"`
	Question string    `json:"question"`
	Keys     []string  `json:"keys"`
	Asked    time.Time `json:"asked"`
}

type Queue struct {
	dir      string
	capacity int

	mu      sync.Mutex
	pending []*Task
	parked  map[string]*parked // by task ID
	notify  chan struct{}
}

func Open(dir string, capacity int) (*Queue, error) {
	if capacity <= 0 {
		capacity = 256
	}
	q := &Queue{
		dir:      dir,
		capacity: capacity,
		parked:   make(map[string]*parked),
		notify:   make(chan struct{}, 1),
	}
	for _, sub := range []string{"pending", "parked"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("taskqueue: mkdir: %w", err)
		}
	}
	if err := q.load(); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *Queue) load() error {
	files, err := filepath.Glob(filepath.Join(q.dir, "pending", "*.json"))
	if err != nil {
		return fmt.Errorf("taskqueue: glob pending: %w", err)
	}
	for _, f := range files {
		var t Task
		if err := readJSON(f, &t); err != nil {
			slog.Warn("taskqueue: unreadable task; removing", "file", f, "err", err)
			_ = os.Remove(f)
			continue
		}
		if t.Attempts >= maxAttempts {
			slog.Error("taskqueue: task failed too often; dropping",
				"id", t.ID, "attempts", t.Attempts, "source", t.Event.Source)
			_ = os.Remove(f)
			continue
		}
		q.pending = append(q.pending, &t)
	}
	sort.Slice(q.pending, func(i, j int) bool { return q.pending[i].Enqueued.Before(q.pending[j].Enqueued) })

	files, err = filepath.Glob(filepath.Join(q.dir, "parked", "*.json"))
	if err != nil {
		return fmt.Errorf("taskqueue: glob parked: %w", err)
	}
	for _, f := range files {
		var p parked
		if err := readJSON(f, &p); err != nil || time.Since(p.Asked) > ParkTTL {
			_ = os.Remove(f)
			continue
		}
		q.parked[p.TaskID] = &p
	}
	if len(q.pending) > 0 {
		q.signal()
	}
	slog.Info("taskqueue: loaded", "pending", len(q.pending), "parked", len(q.parked))
	return nil
}

// Enqueue consumes the parked record the event answers, if any, into t.Resume.
func (q *Queue) Enqueue(ev event.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) >= q.capacity {
		return ErrFull
	}
	t := &Task{ID: newID(), Event: ev, Enqueued: time.Now()}
	if p := q.matchParked(ev); p != nil {
		t.Resume = &Resume{TaskID: p.TaskID, Request: p.Request, Question: p.Question, Asked: p.Asked}
		delete(q.parked, p.TaskID)
		_ = os.Remove(q.parkedPath(p.TaskID))
		slog.Info("taskqueue: event resumes parked task", "task", t.ID, "parked", p.TaskID)
	}
	if err := writeJSON(q.pendingPath(t), t); err != nil {
		return err
	}
	q.pending = append(q.pending, t)
	q.signal()
	return nil
}

// matchParked finds the parked task ev answers. Reply keys win over actor
// keys; among actor matches the most recent question wins. Caller holds q.mu.
func (q *Queue) matchParked(ev event.Event) *parked {
	for _, key := range ev.ResumeKeys() {
		var best *parked
		for id, p := range q.parked {
			if time.Since(p.Asked) > ParkTTL {
				delete(q.parked, id)
				_ = os.Remove(q.parkedPath(id))
				continue
			}
			for _, k := range p.Keys {
				if k == key && (best == nil || p.Asked.After(best.Asked)) {
					best = p
				}
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// Next leaves the task on disk until Done or Park.
func (q *Queue) Next(ctx context.Context) (*Task, error) {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			t := q.pending[0]
			q.pending = q.pending[1:]
			t.Attempts++
			if err := writeJSON(q.pendingPath(t), t); err != nil {
				slog.Warn("taskqueue: attempt count not persisted", "id", t.ID, "err", err)
			}
			q.mu.Unlock()
			return t, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-q.notify:
		}
	}
}

func (q *Queue) Done(t *Task) {
	if err := os.Remove(q.pendingPath(t)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("taskqueue: remove failed", "id", t.ID, "err", err)
	}
}

func (q *Queue) Park(t *Task, question string, keys []string) error {
	request := t.Event.Content
	if t.Resume != nil {
		// A follow-up question keeps pointing at what the user first asked.
		request = t.Resume.Request
	}
	p := &parked{TaskID: t.ID, Request: request, Question: question, Keys: keys, Asked: time.Now()}
	if err := writeJSON(q.parkedPath(t.ID), p); err != nil {
		return err
	}
	q.mu.Lock()
	q.parked[t.ID] = p
	q.mu.Unlock()
	q.Done(t)
	return nil
}

func (q *Queue) Stats() (pending, parkedN int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending), len(q.parked)
}

func (q *Queue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *Queue) pendingPath(t *Task) string {
	// Enqueue time leads the name so a directory listing reads in order.
	return filepath.Join(q.dir, "pending", fmt.Sprintf("%020d-%s.json", t.Enqueued.UnixNano(), t.ID))
}

func (q *Queue) parkedPath(id string) string {
	return filepath.Join(q.dir, "parked", id+".json")
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "t-" + hex.EncodeToString(b[:])
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("taskqueue: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("taskqueue: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("taskqueue: rename: %w", err)
	}
	return nil
}

func readJSON(path string, v any) error {
	if strings.HasSuffix(path, ".tmp") {
		return errors.New("temp file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
