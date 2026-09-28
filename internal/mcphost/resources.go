package mcphost

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/scope"
)

// maxPinnedBytes bounds what pinned resources add to every system prompt.
const maxPinnedBytes = 32 * 1024

// ResourceInfo describes one readable resource or resource template.
type ResourceInfo struct {
	Server      string
	URI         string // a URI, or a URI template when Template is set
	Template    bool
	Name        string
	Description string
	Pinned      bool
}

// Pinned is one pinned resource's content.
type Pinned struct {
	URI  string
	Text string
}

func capabilities(c *conn) *mcp.ServerCapabilities {
	if init := c.session.InitializeResult(); init != nil {
		return init.Capabilities
	}
	return nil
}

func (h *Host) refreshResources(ctx context.Context, c *conn) error {
	if caps := capabilities(c); caps == nil || caps.Resources == nil {
		return nil
	}
	var res []*mcp.Resource
	for r, err := range c.session.Resources(ctx, nil) {
		if err != nil {
			return fmt.Errorf("mcphost: list resources on %s: %w", c.name, err)
		}
		res = append(res, r)
	}
	var tmpl []*mcp.ResourceTemplate
	for t, err := range c.session.ResourceTemplates(ctx, nil) {
		if err != nil {
			return fmt.Errorf("mcphost: list resource templates on %s: %w", c.name, err)
		}
		tmpl = append(tmpl, t)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].URI < res[j].URI })
	h.mu.Lock()
	c.resources, c.templates = res, tmpl
	h.mu.Unlock()
	return nil
}

func pinned(r *mcp.Resource) bool {
	return r.Annotations != nil && r.Annotations.Priority >= 1
}

// Resources lists every server's resources and templates, sorted by server.
func (h *Host) Resources() []ResourceInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []ResourceInfo
	for _, c := range h.sortedConns() {
		for _, r := range c.resources {
			out = append(out, ResourceInfo{Server: c.name, URI: r.URI, Name: r.Name,
				Description: r.Description, Pinned: pinned(r) && c.opts.Trusted})
		}
		for _, t := range c.templates {
			out = append(out, ResourceInfo{Server: c.name, URI: t.URITemplate, Template: true,
				Name: t.Name, Description: t.Description})
		}
	}
	return out
}

// Pinned reads every pinned resource of every trusted server, in server
// then URI order, stopping at maxPinnedBytes. Untrusted servers cannot pin:
// that would put third-party text in the system prompt (D-038, D-042).
func (h *Host) Pinned(ctx context.Context) ([]Pinned, error) {
	type ref struct {
		c   *conn
		uri string
	}
	h.mu.RLock()
	var refs []ref
	for _, c := range h.sortedConns() {
		if !c.opts.Trusted {
			continue
		}
		for _, r := range c.resources {
			if pinned(r) {
				refs = append(refs, ref{c, r.URI})
			}
		}
	}
	h.mu.RUnlock()

	var out []Pinned
	total := 0
	for _, r := range refs {
		text, err := h.read(ctx, r.c, r.uri)
		if err != nil {
			return out, err
		}
		if total+len(text) > maxPinnedBytes {
			slog.Warn("mcphost: pinned resources exceed the cap; dropping the rest",
				"at", r.uri, "cap_bytes", maxPinnedBytes)
			break
		}
		total += len(text)
		out = append(out, Pinned{URI: r.uri, Text: text})
	}
	return out, nil
}

// ReadResource reads uri from the server that lists it or whose template
// prefix matches it.
func (h *Host) ReadResource(ctx context.Context, uri string) (string, error) {
	c := h.owner(uri)
	if c == nil {
		return "", fmt.Errorf("no server provides %q", uri)
	}
	text, err := h.read(ctx, c, uri)
	if err != nil {
		return "", err
	}
	return truncate(text), nil
}

func (h *Host) owner(uri string) *conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var best *conn
	bestLen := 0
	for _, c := range h.sortedConns() {
		for _, r := range c.resources {
			if r.URI == uri {
				return c
			}
		}
		for _, t := range c.templates {
			// The literal text before the first expression is enough to route:
			// servers claim distinct schemes or paths.
			prefix, _, _ := strings.Cut(t.URITemplate, "{")
			if prefix != "" && strings.HasPrefix(uri, prefix) && len(prefix) > bestLen {
				best, bestLen = c, len(prefix)
			}
		}
	}
	return best
}

func (h *Host) read(ctx context.Context, c *conn, uri string) (string, error) {
	params := &mcp.ReadResourceParams{URI: uri}
	if c.opts.Trusted {
		if sc, ok := scope.From(ctx); ok {
			params.Meta = mcp.Meta{scope.MetaKey: sc.ToMeta()}
		}
	}
	res, err := c.session.ReadResource(ctx, params)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", uri, err)
	}
	parts := make([]string, 0, len(res.Contents))
	for _, rc := range res.Contents {
		if rc == nil {
			continue
		}
		if rc.Text != "" {
			parts = append(parts, rc.Text)
		} else if len(rc.Blob) > 0 {
			parts = append(parts, fmt.Sprintf("[binary: %s, %d bytes]", rc.MIMEType, len(rc.Blob)))
		}
	}
	return strings.Join(parts, "\n"), nil
}

// sortedConns returns connections by name. Caller holds h.mu.
func (h *Host) sortedConns() []*conn {
	out := make([]*conn, 0, len(h.conns))
	for _, c := range h.conns {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
