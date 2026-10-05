package mcphost

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
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
	return r.Annotations != nil && r.Annotations.Priority > 0
}

func pinPriority(r *mcp.Resource) float64 {
	if r.Annotations == nil {
		return 0
	}
	return r.Annotations.Priority
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

// Pinned reads every pinned resource of every trusted server, highest
// priority first and then by server and URI, stopping at maxPinnedBytes.
// Priority orders the system prompt, so text that never changes precedes text
// that changes per turn and the stable prefix stays cacheable (D-017, D-042).
// Untrusted servers cannot pin: that would put third-party text in the system
// prompt (D-038).
func (h *Host) Pinned(ctx context.Context) ([]Pinned, error) {
	type ref struct {
		c        *conn
		uri      string
		priority float64
	}
	h.mu.RLock()
	var refs []ref
	for _, c := range h.sortedConns() {
		if !c.opts.Trusted {
			continue
		}
		for _, r := range c.resources {
			if pinned(r) {
				refs = append(refs, ref{c, r.URI, pinPriority(r)})
			}
		}
	}
	h.mu.RUnlock()
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].priority > refs[j].priority })

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
// prefix matches it. A #L.. fragment selects line ranges and is handled here,
// never sent on: MCP has no range parameter, so a server — ours or a third
// party's — needs no knowledge of it (D-056).
func (h *Host) ReadResource(ctx context.Context, uri string) (string, error) {
	base, ranges, err := cutRanges(uri)
	if err != nil {
		return "", err
	}
	c := h.owner(base)
	if c == nil {
		return "", fmt.Errorf("no server provides %q", base)
	}
	text, err := h.read(ctx, c, base)
	if err != nil {
		return "", err
	}
	if len(ranges) > 0 {
		text = sliceRanges(base, text, ranges)
	}
	return truncate(text), nil
}

// cutRanges splits "uri#L10-20,L40" into the bare URI and its spans. A
// fragment that isn't a line range is an error rather than a silent whole-file
// read, which would quietly blow the result cap.
func cutRanges(uri string) (string, []LineRange, error) {
	base, frag, ok := strings.Cut(uri, "#")
	if !ok || strings.TrimSpace(frag) == "" {
		return uri, nil, nil
	}
	var out []LineRange
	for _, part := range strings.Split(frag, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "L") && !strings.HasPrefix(part, "l") {
			return "", nil, fmt.Errorf("unknown fragment %q: use #L<line> or #L<from>-<to>, comma-separated", frag)
		}
		nums := strings.SplitN(part[1:], "-", 2)
		from, err := strconv.Atoi(strings.TrimSpace(nums[0]))
		if err != nil || from < 1 {
			return "", nil, fmt.Errorf("bad line number in %q", part)
		}
		to := from
		if len(nums) == 2 {
			if to, err = strconv.Atoi(strings.TrimSpace(nums[1])); err != nil || to < from {
				return "", nil, fmt.Errorf("bad range in %q", part)
			}
		}
		out = append(out, LineRange{From: from, To: to})
	}
	return base, out, nil
}

// LineRange is an inclusive 1-based span of lines.
type LineRange struct {
	From int
	To   int
}

func (r LineRange) String() string {
	if r.From == r.To {
		return fmt.Sprintf("L%d", r.From)
	}
	return fmt.Sprintf("L%d-%d", r.From, r.To)
}

// sliceRanges returns only the requested spans, each under a header saying
// where it came from so a follow-up range can be derived from it.
func sliceRanges(base, text string, ranges []LineRange) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for _, r := range ranges {
		from, to := r.From, r.To
		if to > len(lines) {
			to = len(lines)
		}
		if from > len(lines) || to < from {
			fmt.Fprintf(&b, "%s#%s  (file has %d lines)\n", base, r.String(), len(lines))
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s#%s\n%s\n", base, LineRange{From: from, To: to}.String(),
			strings.Join(lines[from-1:to], "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
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
