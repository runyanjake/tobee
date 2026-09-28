// Package memory is the built-in "memory" MCP server over the memory sandbox,
// scoped per call to the turn's user, shared, or both (search/list only).
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/datedname"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/sandboxfs"
	"github.com/runyanjake/tobee/internal/scope"
)

const (
	sharedRoot = "shared"
	uriPrefix  = "memory://"
)

func New(instructions string, fs *sandboxfs.FS) *mcpserver.Server {
	srv := mcpserver.New("memory", instructions)
	srv.AddResourceTemplate(mcpserver.ResourceTemplate{
		URITemplate: uriPrefix + "{scope}/{+path}",
		Name:        "memory",
		Description: `Memory files. scope is "user" (the current user's tree) or "shared". ` +
			`memory_list and memory_search return these URIs.`,
		MIMEType: "text/markdown",
		Read:     readResource(fs),
	})

	srv.Add(mcpserver.Tool{
		Name: "write",
		Description: `Create or overwrite a memory file. Pass the filename you want; the backend ` +
			`prepends today's date and kebab-cases the name. "My Notes.md" becomes ` +
			`"YYYY.MM.DD-my-notes.md". Subdirectories are preserved. scope="user" (default) ` +
			`writes to the active user's tree; scope="shared" writes to cross-user knowledge.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Filename (with optional subdir). Date prefix and kebab-case applied automatically. Do not add a date yourself."},
				"content": {"type": "string", "description": "Full file contents"},
				"scope":   {"type": "string", "enum": ["user", "shared"], "description": "Default \"user\"."}
			},
			"required": ["path", "content"]
		}`),
		Handler: writeHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name: "append",
		Description: `Append content to a memory file, creating it if needed. Filename is ` +
			`auto-stamped with today's date and kebab-cased the first time it is created; ` +
			`subsequent appends in the same turn target the same dated file. Prefer this over ` +
			`memory_write when adding to a list or journal.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Filename (with optional subdir). Date prefix and kebab-case applied automatically."},
				"content": {"type": "string", "description": "Text to append (include leading newline if needed)"},
				"scope":   {"type": "string", "enum": ["user", "shared"], "description": "Default \"user\"."}
			},
			"required": ["path", "content"]
		}`),
		Handler: appendHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name:        "search",
		Description: `Case-insensitive substring search. Returns "<uri>:<line>  <snippet>" rows; read a hit with resources_read. Default scope is "both" (user + shared).`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search term"},
				"limit": {"type": "integer", "description": "Max hits to return (default 20)"},
				"scope": {"type": "string", "enum": ["user", "shared", "both"], "description": "Default \"both\"."}
			},
			"required": ["query"]
		}`),
		ReadOnly: true,
		Handler:  searchHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name:        "list",
		Description: `List memory files as memory:// URIs; read one with resources_read. Default scope is "both" (user + shared).`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"dir":   {"type": "string", "description": "Optional subdirectory relative to the scope root"},
				"scope": {"type": "string", "enum": ["user", "shared", "both"], "description": "Default \"both\"."}
			}
		}`),
		ReadOnly: true,
		Handler:  listHandler(fs),
	})
	return srv
}

type scopedRoot struct {
	Label string // "user" | "shared"
	Dir   string // FS-relative root for this scope
}

// writableRoot errors on scope=user with no user attached rather than writing somewhere generic.
func writableRoot(ctx context.Context, scopeArg string) (scopedRoot, error) {
	switch scopeArg {
	case "", "user":
		s, ok := scope.From(ctx)
		if !ok || !s.HasUser() {
			return scopedRoot{}, fmt.Errorf(`user scope unavailable on this turn; pass scope="shared" instead`)
		}
		return scopedRoot{Label: "user", Dir: s.Dir()}, nil
	case "shared":
		return scopedRoot{Label: "shared", Dir: sharedRoot}, nil
	default:
		return scopedRoot{}, fmt.Errorf(`invalid scope %q (expected "user" or "shared")`, scopeArg)
	}
}

// readableRoots: "both" is shared plus the user's tree when a user is attached.
func readableRoots(ctx context.Context, scopeArg string) ([]scopedRoot, error) {
	switch scopeArg {
	case "", "both":
		roots := []scopedRoot{{Label: "shared", Dir: sharedRoot}}
		if s, ok := scope.From(ctx); ok && s.HasUser() {
			roots = append(roots, scopedRoot{Label: "user", Dir: s.Dir()})
		}
		return roots, nil
	case "user", "shared":
		r, err := writableRoot(ctx, scopeArg)
		if err != nil {
			return nil, err
		}
		return []scopedRoot{r}, nil
	default:
		return nil, fmt.Errorf(`invalid scope %q (expected "user", "shared", or "both")`, scopeArg)
	}
}

// joinScope confines p to the scope root. sandboxfs only keeps paths inside
// the whole memory tree, so "../<other user>" must be stopped here (D-013).
func joinScope(root scopedRoot, p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return root.Dir, nil
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q leaves the %s scope", p, root.Label)
	}
	return root.Dir + "/" + c, nil
}

// uri renders a scope-relative path as memory://<scope>/<path>.
func uri(label, rel string) string { return uriPrefix + label + "/" + rel }

func readResource(fs *sandboxfs.FS) func(context.Context, string) (string, error) {
	return func(ctx context.Context, u string) (string, error) {
		label, rel, ok := strings.Cut(strings.TrimPrefix(u, uriPrefix), "/")
		if !ok || rel == "" {
			return "", fmt.Errorf("%q is not memory://<scope>/<path>", u)
		}
		root, err := writableRoot(ctx, label)
		if err != nil {
			return "", err
		}
		full, err := joinScope(root, rel)
		if err != nil {
			return "", err
		}
		return fs.Read(full)
	}
}

func writeHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		root, err := writableRoot(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		dated, err := datedname.Apply(in.Path, time.Now())
		if err != nil {
			return "", err
		}
		full, err := joinScope(root, dated)
		if err != nil {
			return "", err
		}
		if err := fs.Write(full, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s (%d bytes)", uri(root.Label, dated), len(in.Content)), nil
	}
}

func appendHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		root, err := writableRoot(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		dated, err := datedname.Apply(in.Path, time.Now())
		if err != nil {
			return "", err
		}
		full, err := joinScope(root, dated)
		if err != nil {
			return "", err
		}
		if err := fs.Append(full, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("appended to %s (%d bytes)", uri(root.Label, dated), len(in.Content)), nil
	}
}

func searchHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		roots, err := readableRoots(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		var sb strings.Builder
		remaining := limit
		for _, r := range roots {
			if remaining <= 0 {
				break
			}
			hits, err := fs.SearchUnder(in.Query, remaining, r.Dir)
			if err != nil {
				continue // missing dirs are fine; the FS swallows ENOENT
			}
			for _, h := range hits {
				rel := strings.TrimPrefix(h.Path, r.Dir+"/")
				fmt.Fprintf(&sb, "%s:%d  %s\n", uri(r.Label, rel), h.Line, h.Snippet)
				remaining--
				if remaining <= 0 {
					break
				}
			}
		}
		out := strings.TrimRight(sb.String(), "\n")
		if out == "" {
			return "no matches", nil
		}
		return out, nil
	}
}

func listHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Dir   string `json:"dir"`
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(args, &in)
		roots, err := readableRoots(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, r := range roots {
			start, err := joinScope(r, in.Dir)
			if err != nil {
				return "", err
			}
			files, err := fs.List(start)
			if err != nil {
				continue
			}
			for _, f := range files {
				rel := strings.TrimPrefix(f, r.Dir+"/")
				fmt.Fprintf(&sb, "%s\n", uri(r.Label, rel))
			}
		}
		out := strings.TrimRight(sb.String(), "\n")
		if out == "" {
			return "(empty)", nil
		}
		return out, nil
	}
}
