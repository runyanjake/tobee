// Package ingest is tobee's input engine. Sources — a Discord gateway, an
// IMAP poller, the job scheduler, an MCP resource subscription — emit
// normalized events; the engine supervises them, drops duplicates and
// events from senders a source does not admit, and hands the rest to the
// task queue (D-034).
//
// Sources can be registered and unregistered while tobee runs.
package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runyanjake/tobee/internal/event"
)

// Emit hands one event to the engine. Safe to call from any goroutine.
type Emit func(event.Event)

// Source produces events. Run blocks until ctx is cancelled. Returning
// early with an error makes the engine restart the source with backoff;
// returning nil after ctx is done is a clean stop.
type Source interface {
	Name() string
	Run(ctx context.Context, emit Emit) error
}

// Sink receives admitted events. The task queue implements it.
type Sink interface {
	Enqueue(event.Event) error
}

// Backoff bounds for restarting a failed source.
const (
	minBackoff = time.Second
	maxBackoff = time.Minute
	// A source that ran this long before failing gets its backoff reset.
	healthyRun = time.Minute
)

// dedupSize bounds the remembered event IDs. Pollers re-see a message at
// most a few polls apart, so a short memory is enough.
const dedupSize = 1024

// Engine supervises sources and admits their events.
type Engine struct {
	sink Sink

	mu      sync.Mutex
	ctx     context.Context // set by Start; nil until then
	sources map[string]*running
	allow   map[string]map[string]bool // source → admitted actor IDs; absent = admit all
	seen    map[string]bool
	order   []string // dedup FIFO
}

type running struct {
	src    Source
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	up       bool
	restarts int
	admitted int
	dropped  int
	lastErr  string
	lastAt   time.Time
}

// New creates an engine that delivers admitted events to sink.
func New(sink Sink) *Engine {
	return &Engine{
		sink:    sink,
		sources: make(map[string]*running),
		allow:   make(map[string]map[string]bool),
		seen:    make(map[string]bool),
	}
}

// Allow restricts a source to events from the given actor IDs. Events
// with no actor (timers, notifications) are unaffected. An empty list
// removes the restriction.
func (e *Engine) Allow(source string, actorIDs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(actorIDs) == 0 {
		delete(e.allow, source)
		return
	}
	set := make(map[string]bool, len(actorIDs))
	for _, id := range actorIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	e.allow[source] = set
}

// Register adds a source. If the engine is running the source starts
// immediately; otherwise it starts with Start.
func (e *Engine) Register(src Source) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := src.Name()
	if _, dup := e.sources[name]; dup {
		return fmt.Errorf("ingest: source %q already registered", name)
	}
	r := &running{src: src}
	e.sources[name] = r
	if e.ctx != nil {
		e.launch(r)
	}
	slog.Info("ingest: source registered", "source", name, "running", e.ctx != nil)
	return nil
}

// Unregister stops a source and waits for it to exit.
func (e *Engine) Unregister(name string) error {
	e.mu.Lock()
	r, ok := e.sources[name]
	delete(e.sources, name)
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("ingest: source %q not registered", name)
	}
	if r.cancel != nil {
		r.cancel()
		<-r.done
	}
	slog.Info("ingest: source unregistered", "source", name)
	return nil
}

// Start launches every registered source. Cancel ctx to stop them all;
// Wait blocks until they have exited.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ctx = ctx
	for _, r := range e.sources {
		e.launch(r)
	}
}

// Wait blocks until every running source has exited.
func (e *Engine) Wait() {
	e.mu.Lock()
	var dones []chan struct{}
	for _, r := range e.sources {
		if r.done != nil {
			dones = append(dones, r.done)
		}
	}
	e.mu.Unlock()
	for _, d := range dones {
		<-d
	}
}

// launch starts r's supervisor. Caller holds e.mu.
func (e *Engine) launch(r *running) {
	ctx, cancel := context.WithCancel(e.ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	go e.supervise(ctx, r)
}

func (e *Engine) supervise(ctx context.Context, r *running) {
	defer close(r.done)
	name := r.src.Name()
	emit := func(ev event.Event) { e.admit(r, ev) }
	backoff := minBackoff
	for {
		r.setUp(true, "")
		started := time.Now()
		err := r.src.Run(ctx, emit)
		r.setUp(false, errString(err))
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > healthyRun {
			backoff = minBackoff
		}
		slog.Error("ingest: source stopped; restarting",
			"source", name, "err", err, "backoff", backoff)
		r.mu.Lock()
		r.restarts++
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// admit applies dedup and the sender allowlist, then enqueues.
func (e *Engine) admit(r *running, ev event.Event) {
	name := r.src.Name()
	if ev.Source == "" {
		ev.Source = name
	}
	if ev.ID == "" {
		ev.ID = randomID()
	}
	if ev.Received.IsZero() {
		ev.Received = time.Now()
	}

	e.mu.Lock()
	key := ev.Source + "\x00" + ev.ID
	duplicate := e.seen[key]
	if !duplicate {
		e.remember(key)
	}
	allowed := true
	if set, ok := e.allow[ev.Source]; ok && ev.Actor.ID != "" {
		allowed = set[strings.ToLower(ev.Actor.ID)]
	}
	e.mu.Unlock()

	if duplicate {
		slog.Debug("ingest: duplicate event dropped", "source", ev.Source, "id", ev.ID)
		return
	}
	if !allowed {
		r.count(false)
		slog.Warn("ingest: sender not admitted; dropping",
			"source", ev.Source, "actor", ev.Actor.ID, "actor_name", ev.Actor.Name)
		return
	}
	if err := e.sink.Enqueue(ev); err != nil {
		r.count(false)
		slog.Error("ingest: enqueue failed; dropping", "source", ev.Source, "id", ev.ID, "err", err)
		return
	}
	r.count(true)
	slog.Debug("ingest: event admitted", "source", ev.Source, "id", ev.ID, "kind", ev.Kind,
		"connector", ev.Origin.Connector, "channel", ev.Origin.Channel, "actor", ev.Actor.ID)
}

// remember records key in the bounded dedup set. Caller holds e.mu.
func (e *Engine) remember(key string) {
	e.seen[key] = true
	e.order = append(e.order, key)
	if len(e.order) > dedupSize {
		delete(e.seen, e.order[0])
		e.order = e.order[1:]
	}
}

// Names returns the registered source names, sorted.
func (e *Engine) Names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.sources))
	for n := range e.sources {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *running) setUp(up bool, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.up = up
	if lastErr != "" {
		r.lastErr = lastErr
	}
}

func (r *running) count(admitted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if admitted {
		r.admitted++
		r.lastAt = time.Now()
	} else {
		r.dropped++
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
