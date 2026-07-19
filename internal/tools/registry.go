package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
)

// Handler executes a tool call. Arguments arrive as a JSON object matching
// the tool's InputSchema; the returned string becomes the tool's content
// block in the next LLM turn.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Spec describes a callable tool. Name must be unique within a Registry.
type Spec struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for Arguments
	Timeout     time.Duration   // 0 means 30s default
	Handler     Handler

	// Verbatim marks a tool whose output is already the user-facing
	// answer, rendered deterministically by the tool itself. The agent
	// attaches that output to the reply in code; the model never gets
	// the chance to reword it. Asking a model to copy text is not a
	// guarantee — enforcing it here is (D-030).
	Verbatim bool
}

// Registry is the agent's central catalogue of callable tools.
type Registry struct {
	mu    sync.RWMutex
	specs map[string]Spec
}

func NewRegistry() *Registry {
	return &Registry{specs: make(map[string]Spec)}
}

// Register adds a tool. Returns error if the name is already in use.
func (r *Registry) Register(spec Spec) error {
	if spec.Name == "" {
		return fmt.Errorf("tool name must not be empty")
	}
	if spec.Handler == nil {
		return fmt.Errorf("tool %q: handler required", spec.Name)
	}
	if len(spec.InputSchema) == 0 {
		// Permit schemaless tools, but give the model *something* to serialise.
		spec.InputSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	}
	if spec.Timeout == 0 {
		spec.Timeout = 30 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.specs[spec.Name]; exists {
		return fmt.Errorf("tool %q already registered", spec.Name)
	}
	r.specs[spec.Name] = spec
	return nil
}

// MustRegister panics on registration failure. For use in main wiring where
// duplicate registration is a programmer error.
func (r *Registry) MustRegister(spec Spec) {
	if err := r.Register(spec); err != nil {
		panic(err)
	}
}

// Specs returns tool specs for inclusion in an LLM call, sorted by name.
func (r *Registry) Specs() []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.ToolSpec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, llm.ToolSpec{
			Name:        s.Name,
			Description: s.Description,
			InputSchema: s.InputSchema,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.specs))
	for n := range r.specs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsVerbatim reports whether the named tool's output is final
// user-facing text. Unknown names report false.
func (r *Registry) IsVerbatim(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.specs[name].Verbatim
}

// Call executes a registered tool by name. Wraps the handler with a timeout
// and recovers panics into errors so one bad tool cannot kill the loop.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (result string, err error) {
	r.mu.RLock()
	spec, ok := r.specs[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tool not found: %q", name)
	}

	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	tctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			slog.Error("tool panic", "tool", name, "panic", r)
			err = fmt.Errorf("tool %q panicked: %v", name, r)
		}
	}()

	return spec.Handler(tctx, args)
}
