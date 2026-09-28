// Package mcpserver builds tobee's built-in MCP servers. Each built-in
// capability (memory, workspace, schedule, status, user, and the
// connectors' own tools) is an ordinary MCP server that the MCP host
// connects to over an in-memory transport, exactly as it would connect to
// an external server over stdio or HTTP (D-033).
//
// The package keeps tool definitions close to the old registry shape — a
// name, a description, a JSON Schema, and a string-returning handler — and
// handles the MCP plumbing: scope re-attachment from `_meta`, error
// results, panic recovery, and the tobee-specific result metadata.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/scope"
)

// Metadata keys tobee reads from trusted servers. External servers can
// set them too; the host ignores them unless the server is trusted (D-038).
const (
	// MetaVerbatim on a tool definition marks its output as finished,
	// user-facing text that the agent appends to the reply in code (D-030).
	MetaVerbatim = "tobee/verbatim"
	// MetaAwait on a tool result pauses the task until the user answers.
	// The value is an Await (D-036).
	MetaAwait = "tobee/await"
)

// Await is the MetaAwait payload: the question that was sent and the
// keys an answering event may match (see event.ReplyKey, event.ActorKey).
type Await struct {
	Question string   `json:"question"`
	Keys     []string `json:"keys"`
}

// Handler executes a tool call. Arguments arrive as a JSON object matching
// the tool's InputSchema; the returned string becomes the tool's content
// block in the next LLM turn. A returned error becomes an error result the
// model sees, not a protocol error.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool describes one tool a built-in server exposes.
type Tool struct {
	Name        string // bare name; the host namespaces it as <server>_<name>
	Description string
	InputSchema json.RawMessage // JSON Schema, type "object"
	Handler     Handler

	// ReadOnly sets the MCP readOnlyHint annotation.
	ReadOnly bool
	// Verbatim marks output as already user-facing (see MetaVerbatim).
	Verbatim bool
}

// Server is one built-in MCP server.
type Server struct {
	name string
	srv  *mcp.Server

	mu    sync.Mutex
	names map[string]bool
}

// New creates a server. instructions become the MCP `instructions` the
// host shows the model; load them from prompts/servers/<name>.md.
func New(name, instructions string) *Server {
	return &Server{
		name: name,
		srv: mcp.NewServer(
			&mcp.Implementation{Name: name, Version: "1"},
			&mcp.ServerOptions{Instructions: instructions},
		),
		names: make(map[string]bool),
	}
}

// Name is the server name; the host uses it as the tool namespace.
func (s *Server) Name() string { return s.name }

// MCP returns the underlying SDK server for the host to connect to.
func (s *Server) MCP() *mcp.Server { return s.srv }

// Add registers a tool. Duplicate or malformed tools are programmer
// errors and panic, like the registry this replaces.
func (s *Server) Add(t Tool) {
	if t.Name == "" || t.Handler == nil {
		panic(fmt.Sprintf("mcpserver %s: tool needs a name and a handler", s.name))
	}
	s.mu.Lock()
	if s.names[t.Name] {
		s.mu.Unlock()
		panic(fmt.Sprintf("mcpserver %s: tool %q already registered", s.name, t.Name))
	}
	s.names[t.Name] = true
	s.mu.Unlock()

	schema := t.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	def := &mcp.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t.ReadOnly},
	}
	if t.Verbatim {
		def.Meta = mcp.Meta{MetaVerbatim: true}
	}
	s.srv.AddTool(def, s.wrap(t))
}

// resultMeta is the per-call holder SetResultMeta writes into.
type resultMetaKey struct{}

// SetResultMeta attaches a `_meta` entry to the result of the tool call
// running on ctx. Used by tools that signal the agent through metadata,
// such as user_ask setting MetaAwait.
func SetResultMeta(ctx context.Context, key string, value any) {
	if m, ok := ctx.Value(resultMetaKey{}).(mcp.Meta); ok {
		m[key] = value
	}
}

func (s *Server) wrap(t Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		if sc, ok := scope.FromMeta(req.Params.Meta); ok {
			ctx = scope.With(ctx, sc)
		}
		meta := mcp.Meta{}
		ctx = context.WithValue(ctx, resultMetaKey{}, meta)

		// One bad tool must not take down the server session.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("mcpserver: tool panic", "server", s.name, "tool", t.Name, "panic", r)
				res = errorResult(fmt.Errorf("tool %q panicked: %v", t.Name, r))
				err = nil
			}
		}()

		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out, herr := t.Handler(ctx, args)
		if herr != nil {
			return errorResult(herr), nil
		}
		res = &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}
		if len(meta) > 0 {
			res.Meta = meta
		}
		return res, nil
	}
}

func errorResult(err error) *mcp.CallToolResult {
	r := &mcp.CallToolResult{}
	r.SetError(err)
	return r
}
