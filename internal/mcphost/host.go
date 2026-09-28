// Package mcphost holds one MCP client session per connected server and
// presents their tools as one <server>_<tool> catalog (D-033); trust is per server (D-038).
package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/scope"
)

const defaultTimeout = 30 * time.Second

// An external server can return anything; an unbounded result would blow the context.
const maxResultBytes = 32 * 1024

type Options struct {
	Trusted bool
	Timeout time.Duration // per tools/call; 0 means 30s
}

type ServerInfo struct {
	Name         string
	Trusted      bool
	Instructions string   // empty for untrusted servers
	Tools        []string // exposed names, sorted
}

type Result struct {
	Text    string
	IsError bool
	// Set only for trusted servers that marked the output as finished user-facing text (D-030).
	Verbatim bool
	// Set when a trusted server asks to pause the task until the user answers (D-036).
	Await *mcpserver.Await
}

type conn struct {
	name         string
	opts         Options
	session      *mcp.ClientSession
	instructions string
}

type toolRef struct {
	conn     *conn
	tool     *mcp.Tool
	verbatim bool
}

type Host struct {
	client *mcp.Client

	mu       sync.RWMutex
	conns    map[string]*conn            // server name → connection
	tools    map[string]toolRef          // exposed name → tool
	byServer map[string][]string         // server name → exposed names
	watchers map[string]func(uri string) // server name → resources/updated handler
	stats    map[string]*callStat        // server name → counters
}

type callStat struct {
	calls, errors int
	last          time.Time
}

func New() *Host {
	h := &Host{
		conns:    make(map[string]*conn),
		tools:    make(map[string]toolRef),
		byServer: make(map[string][]string),
		watchers: make(map[string]func(string)),
		stats:    make(map[string]*callStat),
	}
	h.client = mcp.NewClient(&mcp.Implementation{Name: "tobee", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			// Handlers run on the session's read loop; refreshing inline
			// would deadlock waiting for our own tools/list response.
			go h.refreshSession(context.Background(), req.Session)
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			h.resourceUpdated(req.Session, req.Params.URI)
		},
		Capabilities: &mcp.ClientCapabilities{},
	})
	return h
}

// watch routes a server's resources/updated notifications to f; nil stops routing.
func (h *Host) watch(server string, f func(uri string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f == nil {
		delete(h.watchers, server)
		return
	}
	h.watchers[server] = f
}

// ConnectInProcess connects a built-in server in memory; built-ins are always trusted.
func (h *Host) ConnectInProcess(ctx context.Context, s *mcpserver.Server) error {
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, serverT, nil); err != nil {
		return fmt.Errorf("mcphost: start %s: %w", s.Name(), err)
	}
	return h.connect(ctx, s.Name(), clientT, Options{Trusted: true})
}

// Connect connects an external server over a CommandTransport or StreamableClientTransport.
func (h *Host) Connect(ctx context.Context, name string, t mcp.Transport, opts Options) error {
	return h.connect(ctx, name, t, opts)
}

func (h *Host) connect(ctx context.Context, name string, t mcp.Transport, opts Options) error {
	if name == "" || sanitize(name) != name {
		return fmt.Errorf("mcphost: invalid server name %q (use [a-z0-9_-])", name)
	}
	h.mu.RLock()
	_, dup := h.conns[name]
	h.mu.RUnlock()
	if dup {
		return fmt.Errorf("mcphost: server %q already connected", name)
	}

	session, err := h.client.Connect(ctx, t, nil)
	if err != nil {
		return fmt.Errorf("mcphost: connect %s: %w", name, err)
	}
	c := &conn{name: name, opts: opts, session: session}
	if init := session.InitializeResult(); init != nil {
		c.instructions = strings.TrimSpace(init.Instructions)
	}

	h.mu.Lock()
	h.conns[name] = c
	h.stats[name] = &callStat{}
	h.mu.Unlock()

	if err := h.refresh(ctx, c); err != nil {
		_ = h.Disconnect(name)
		return err
	}
	slog.Info("mcphost: connected", "server", name, "trusted", opts.Trusted,
		"tools", len(h.byServer[name]))
	return nil
}

func (h *Host) Disconnect(name string) error {
	h.mu.Lock()
	c, ok := h.conns[name]
	if ok {
		delete(h.conns, name)
		for _, exposed := range h.byServer[name] {
			delete(h.tools, exposed)
		}
		delete(h.byServer, name)
		delete(h.stats, name)
	}
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("mcphost: server %q not connected", name)
	}
	slog.Info("mcphost: disconnected", "server", name)
	return c.session.Close()
}

func (h *Host) Close() {
	h.mu.RLock()
	names := make([]string, 0, len(h.conns))
	for n := range h.conns {
		names = append(names, n)
	}
	h.mu.RUnlock()
	for _, n := range names {
		if err := h.Disconnect(n); err != nil {
			slog.Warn("mcphost: close failed", "server", n, "err", err)
		}
	}
}

// Session exposes a server's client session for the resource-subscription source.
func (h *Host) Session(name string) (*mcp.ClientSession, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[name]
	if !ok {
		return nil, false
	}
	return c.session, true
}

func (h *Host) refreshSession(ctx context.Context, s *mcp.ClientSession) {
	h.mu.RLock()
	var target *conn
	for _, c := range h.conns {
		if c.session == s {
			target = c
			break
		}
	}
	h.mu.RUnlock()
	if target == nil {
		return
	}
	if err := h.refresh(ctx, target); err != nil {
		slog.Warn("mcphost: tool list refresh failed", "server", target.name, "err", err)
	}
}

func (h *Host) refresh(ctx context.Context, c *conn) error {
	var listed []*mcp.Tool
	for t, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("mcphost: list tools on %s: %w", c.name, err)
		}
		listed = append(listed, t)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, exposed := range h.byServer[c.name] {
		delete(h.tools, exposed)
	}
	names := make([]string, 0, len(listed))
	for _, t := range listed {
		exposed := exposedName(c.name, t.Name)
		if prev, clash := h.tools[exposed]; clash {
			slog.Warn("mcphost: tool name collision; skipping",
				"tool", exposed, "server", c.name, "existing_server", prev.conn.name)
			continue
		}
		verbatim, _ := t.Meta[mcpserver.MetaVerbatim].(bool)
		h.tools[exposed] = toolRef{conn: c, tool: t, verbatim: verbatim && c.opts.Trusted}
		names = append(names, exposed)
	}
	sort.Strings(names)
	h.byServer[c.name] = names
	return nil
}

func (h *Host) Tools() []llm.ToolSpec {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]llm.ToolSpec, 0, len(h.tools))
	for exposed, ref := range h.tools {
		schema, err := json.Marshal(ref.tool.InputSchema)
		if err != nil || string(schema) == "null" {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		out = append(out, llm.ToolSpec{
			Name:        exposed,
			Description: ref.tool.Description,
			InputSchema: schema,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *Host) ToolNames() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.tools))
	for n := range h.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (h *Host) Servers() []ServerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ServerInfo, 0, len(h.conns))
	for name, c := range h.conns {
		info := ServerInfo{
			Name:    name,
			Trusted: c.opts.Trusted,
			Tools:   append([]string(nil), h.byServer[name]...),
		}
		// Untrusted instructions would be third-party text in the system
		// prompt — the most privileged place a prompt injection can land.
		if c.opts.Trusted {
			info.Instructions = c.instructions
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Call runs a tool by its exposed name. Tool failures come back as Result.IsError;
// err is for calls that never reached a tool (unknown name, transport, timeout).
func (h *Host) Call(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	h.mu.RLock()
	ref, ok := h.tools[name]
	h.mu.RUnlock()
	if !ok {
		return Result{}, fmt.Errorf("tool not found: %q", name)
	}
	c := ref.conn

	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params := &mcp.CallToolParams{Name: ref.tool.Name, Arguments: args}
	if c.opts.Trusted {
		if sc, ok := scope.From(ctx); ok {
			params.Meta = mcp.Meta{scope.MetaKey: sc.ToMeta()}
		}
	}

	timeout := c.opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := c.session.CallTool(cctx, params)
	h.record(c.name, err != nil || (res != nil && res.IsError))
	if err != nil {
		return Result{}, fmt.Errorf("call %s: %w", name, err)
	}

	out := Result{Text: truncate(renderContent(res.Content)), IsError: res.IsError}
	if res.IsError {
		return out, nil
	}
	out.Verbatim = ref.verbatim
	if c.opts.Trusted {
		out.Await = decodeAwait(res.Meta)
	}
	return out, nil
}

func (h *Host) record(server string, failed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.stats[server]
	if !ok {
		return
	}
	st.calls++
	if failed {
		st.errors++
	}
	st.last = time.Now()
}

func (h *Host) resourceUpdated(s *mcp.ClientSession, uri string) {
	h.mu.RLock()
	var f func(string)
	for name, c := range h.conns {
		if c.session == s {
			f = h.watchers[name]
			break
		}
	}
	h.mu.RUnlock()
	if f != nil {
		// Off the read loop: the watcher calls back into the session to read the resource.
		go f(uri)
	}
}

func decodeAwait(meta mcp.Meta) *mcpserver.Await {
	raw, ok := meta[mcpserver.MetaAwait]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var a mcpserver.Await
	if err := json.Unmarshal(b, &a); err != nil || len(a.Keys) == 0 {
		return nil
	}
	return &a
}

// Non-text blocks are named rather than dropped so the model knows they existed.
func renderContent(blocks []mcp.Content) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image: %s]", v.MIMEType))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio: %s]", v.MIMEType))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource: %s]", v.URI))
		case *mcp.EmbeddedResource:
			if v.Resource != nil && v.Resource.Text != "" {
				parts = append(parts, v.Resource.Text)
			} else if v.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource: %s]", v.Resource.URI))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	return s[:maxResultBytes] + fmt.Sprintf("\n[truncated: %d of %d bytes shown]", maxResultBytes, len(s))
}

// Hosted backends enforce the OpenAI function-name pattern ^[a-zA-Z0-9_-]{1,64}$.
func exposedName(server, tool string) string {
	n := server + "_" + sanitize(tool)
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
