// Package mcpserver builds tobee's built-in MCP servers, which the host connects to
// in memory exactly like external ones (D-033).
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/scope"
)

// The host honors these metadata keys only from trusted servers (D-038).
const (
	// On a tool definition: output is user-facing text appended to the reply in code (D-030).
	MetaVerbatim = "tobee/verbatim"
	// On a tool result: pause the task until the user answers; the value is an Await (D-036).
	MetaAwait = "tobee/await"
)

// Keys are the resume keys an answering event may match (event.ReplyKey, event.ActorKey).
type Await struct {
	Question string   `json:"question"`
	Keys     []string `json:"keys"`
}

// Handler returns the tool's text; a returned error becomes an error result the model sees.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

type Tool struct {
	Name        string // bare name; the host namespaces it as <server>_<name>
	Description string
	InputSchema json.RawMessage // JSON Schema, type "object"
	Handler     Handler

	ReadOnly bool
	Verbatim bool
}

type Server struct {
	name string
	srv  *mcp.Server

	mu    sync.Mutex
	names map[string]bool
}

// New takes instructions loaded from prompts/servers/<name>.md.
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

func (s *Server) Name() string { return s.name }

func (s *Server) MCP() *mcp.Server { return s.srv }

// Add panics on duplicate or malformed tools: they are programmer errors.
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

type resultMetaKey struct{}

// SetResultMeta attaches a `_meta` entry to the result of the tool call running on ctx.
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

// Resource is one fixed resource. Pinned resources are placed in every
// system prompt by the host (D-042); Read is called on each read.
type Resource struct {
	URI         string
	Name        string
	Description string
	MIMEType    string
	Pinned      bool
	Read        func(ctx context.Context) (string, error)
}

// ResourceTemplate addresses a family of resources by URI template, e.g.
// memory://{scope}/{+path}. Read receives the concrete URI.
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Description string
	MIMEType    string
	Read        func(ctx context.Context, uri string) (string, error)
}

// ErrNotFound makes a read return MCP's resource-not-found error.
var ErrNotFound = errors.New("resource not found")

// AddResource registers a fixed resource.
func (s *Server) AddResource(r Resource) {
	def := &mcp.Resource{URI: r.URI, Name: r.Name, Description: r.Description, MIMEType: r.MIMEType}
	if r.Pinned {
		def.Annotations = &mcp.Annotations{Audience: []mcp.Role{"assistant"}, Priority: 1}
	}
	s.srv.AddResource(def, s.wrapRead(r.MIMEType, func(ctx context.Context, _ string) (string, error) {
		return r.Read(ctx)
	}))
}

// AddResourceTemplate registers a resource template.
func (s *Server) AddResourceTemplate(t ResourceTemplate) {
	s.srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: t.URITemplate, Name: t.Name, Description: t.Description, MIMEType: t.MIMEType,
	}, s.wrapRead(t.MIMEType, t.Read))
}

func (s *Server) wrapRead(mime string, read func(context.Context, string) (string, error)) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if sc, ok := scope.FromMeta(req.Params.Meta); ok {
			ctx = scope.With(ctx, sc)
		}
		uri := req.Params.URI
		text, err := read(ctx, uri)
		if errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: text}}}, nil
	}
}
