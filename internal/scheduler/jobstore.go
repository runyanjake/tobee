package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Job has exactly one of Cron (recurring) or At (one-shot). Connector keeps
// the "integration" JSON key so jobs saved before the rename still load.
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Cron      string    `json:"cron,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Prompt    string    `json:"prompt"`
	Connector string    `json:"integration"`
	Channel   string    `json:"channel"`
	Thread    string    `json:"thread,omitempty"`
	User      string    `json:"user,omitempty"`
	UserName  string    `json:"userName,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func (j Job) IsRecurring() bool { return j.Cron != "" }

// JobStore keeps one "<id>.json" file per job, written atomically.
type JobStore struct {
	mu      sync.Mutex
	rootDir string
}

func NewJobStore(rootDir string) (*JobStore, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("jobstore: mkdir %s: %w", rootDir, err)
	}
	return &JobStore{rootDir: rootDir}, nil
}

func NewJobID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "j-" + hex.EncodeToString(b[:])
}

func (s *JobStore) path(id string) string {
	return filepath.Join(s.rootDir, id+".json")
}

// Save overwrites any existing file for j.ID.
func (s *JobStore) Save(j Job) error {
	if j.ID == "" {
		return fmt.Errorf("jobstore: job id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("jobstore: marshal: %w", err)
	}
	final := s.path(j.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("jobstore: write tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("jobstore: rename: %w", err)
	}
	return nil
}

// Delete ignores missing files: cancel and one-shot self-cleanup can race.
func (s *JobStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("jobstore: remove %s: %w", id, err)
	}
	return nil
}

// LoadAll skips unreadable files so one corrupt job can't take down the scheduler.
func (s *JobStore) LoadAll() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Job
	err := filepath.WalkDir(s.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var j Job
		if jerr := json.Unmarshal(data, &j); jerr != nil {
			return nil
		}
		out = append(out, j)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("jobstore: walk: %w", err)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	return out, nil
}
