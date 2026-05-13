package scheduler

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tobee/internal/agent"
)

// Janitor periodically rotates idle session summaries and prunes old
// archive files under data/sessions/. It never touches data/memory/.
// See DECISIONS.md D-010 / D-011 for the rationale.
type Janitor struct {
	sessions    *agent.SessionStore
	rootDir     string
	idleTimeout time.Duration
	archiveTTL  time.Duration
	sweepEvery  time.Duration
}

// NewJanitor builds a janitor. Non-positive timeouts disable that step:
// idleTimeout<=0 skips rotation, archiveTTL<=0 skips archive deletion.
func NewJanitor(sessions *agent.SessionStore, rootDir string,
	idleTimeout, archiveTTL, sweepEvery time.Duration) *Janitor {
	if sweepEvery <= 0 {
		sweepEvery = time.Hour
	}
	return &Janitor{
		sessions:    sessions,
		rootDir:     rootDir,
		idleTimeout: idleTimeout,
		archiveTTL:  archiveTTL,
		sweepEvery:  sweepEvery,
	}
}

// Start launches the sweep goroutine. The first sweep runs immediately so
// a restart catches up on rotations a downtime missed. Cancel ctx to stop.
func (j *Janitor) Start(ctx context.Context) {
	if j.idleTimeout <= 0 && j.archiveTTL <= 0 {
		slog.Info("janitor: disabled (idleTimeout and archiveTTL both non-positive)")
		return
	}
	go func() {
		slog.Info("janitor: started",
			"idleTimeout", j.idleTimeout,
			"archiveTTL", j.archiveTTL,
			"sweepEvery", j.sweepEvery)
		j.sweep()
		ticker := time.NewTicker(j.sweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				slog.Info("janitor: stopped")
				return
			case <-ticker.C:
				j.sweep()
			}
		}
	}()
}

func (j *Janitor) sweep() {
	rotated := 0
	if j.idleTimeout > 0 && j.sessions != nil {
		rotated = j.sessions.SweepIdle()
	}
	pruned := 0
	if j.archiveTTL > 0 {
		pruned = j.pruneArchives()
	}
	j.pruneEmptyDirs()
	if rotated > 0 || pruned > 0 {
		slog.Info("janitor: sweep done", "rotated", rotated, "archivedPruned", pruned)
	} else {
		slog.Debug("janitor: sweep done (no-op)")
	}
}

// pruneArchives walks data/sessions/**/archive/*.md and deletes anything
// older than archiveTTL. Returns the number of files removed.
func (j *Janitor) pruneArchives() int {
	removed := 0
	_ = filepath.WalkDir(j.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Only consider files inside an "archive" directory.
		if filepath.Base(filepath.Dir(path)) != "archive" {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if time.Since(info.ModTime()) <= j.archiveTTL {
			return nil
		}
		if rerr := os.Remove(path); rerr != nil {
			slog.Warn("janitor: archive remove failed", "path", path, "err", rerr)
			return nil
		}
		removed++
		return nil
	})
	return removed
}

// pruneEmptyDirs removes empty subdirectories under rootDir, deepest-first.
// rootDir itself is preserved even if it becomes empty.
func (j *Janitor) pruneEmptyDirs() {
	type entry struct {
		path  string
		depth int
	}
	var dirs []entry
	_ = filepath.WalkDir(j.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path == j.rootDir {
			return nil
		}
		rel, rerr := filepath.Rel(j.rootDir, path)
		if rerr != nil {
			return nil
		}
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		dirs = append(dirs, entry{path: path, depth: depth})
		return nil
	})
	// Deepest first so parents can be removed once their children are gone.
	for i := 0; i < len(dirs); i++ {
		for k := i + 1; k < len(dirs); k++ {
			if dirs[k].depth > dirs[i].depth {
				dirs[i], dirs[k] = dirs[k], dirs[i]
			}
		}
	}
	for _, e := range dirs {
		entries, err := os.ReadDir(e.path)
		if err != nil || len(entries) > 0 {
			continue
		}
		if rerr := os.Remove(e.path); rerr != nil {
			slog.Debug("janitor: empty dir remove failed", "path", e.path, "err", rerr)
		}
	}
}
