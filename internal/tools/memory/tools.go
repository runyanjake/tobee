// Package memory wires the memory.FS into the agent's tool registry as a
// pack of read/write/search/list/append tools.
//
// Every tool takes an optional `scope` argument that selects which slice of
// the memory tree it operates on:
//
//   - "user"   — files under data/memory/users/<integration>/<userId>/
//   - "shared" — files under data/memory/shared/
//   - "both"   — read-only scopes only (search, list); walks user + shared
//
// Defaults: read/write/append → "user". search/list → "both".
//
// The active user is read from the per-turn context (see internal/scope).
// A "user" scope call on a turn with no attached user (e.g. a scheduler tick)
// returns a clear error rather than silently writing to a generic location.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tobee/internal/memory"
	"tobee/internal/scope"
	"tobee/internal/tools"
)

const sharedRoot = "shared"

// Register adds memory.* tools to reg, backed by fs.
func Register(reg *tools.Registry, fs *memory.FS) {
	reg.MustRegister(tools.Spec{
		Name:        "memory.read",
		Description: `Read a memory file. Pass scope="user" (default) for files specific to the current user, or scope="shared" for cross-user knowledge.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":  {"type": "string", "description": "Path relative to the scope root, e.g. \"user.md\" or \"facts/deploy.md\""},
				"scope": {"type": "string", "enum": ["user", "shared"], "description": "Which slice of memory to read. Default \"user\"."}
			},
			"required": ["path"]
		}`),
		Handler: readHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.write",
		Description: `Create or overwrite a memory file. scope="user" (default) writes to the active user's tree; scope="shared" writes to cross-user knowledge.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Path relative to the scope root"},
				"content": {"type": "string", "description": "Full file contents"},
				"scope":   {"type": "string", "enum": ["user", "shared"], "description": "Default \"user\"."}
			},
			"required": ["path", "content"]
		}`),
		Handler: writeHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.append",
		Description: `Append content to a memory file, creating it if needed. Prefer this over memory.write when adding to a list or journal.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Path relative to the scope root"},
				"content": {"type": "string", "description": "Text to append (include leading newline if needed)"},
				"scope":   {"type": "string", "enum": ["user", "shared"], "description": "Default \"user\"."}
			},
			"required": ["path", "content"]
		}`),
		Handler: appendHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.search",
		Description: `Case-insensitive substring search. Returns "<scope>:<path>:<line>  <snippet>" rows. Default scope is "both" (user + shared).`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search term"},
				"limit": {"type": "integer", "description": "Max hits to return (default 20)"},
				"scope": {"type": "string", "enum": ["user", "shared", "both"], "description": "Default \"both\"."}
			},
			"required": ["query"]
		}`),
		Handler: searchHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.list",
		Description: `List memory files. Returns "<scope>:<path>" rows. Default scope is "both" (user + shared).`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"dir":   {"type": "string", "description": "Optional subdirectory relative to the scope root"},
				"scope": {"type": "string", "enum": ["user", "shared", "both"], "description": "Default \"both\"."}
			}
		}`),
		Handler: listHandler(fs),
	})
}

type scopedRoot struct {
	Label string // "user" | "shared"
	Dir   string // FS-relative root for this scope
}

// writableRoot returns the FS-relative root for a write/read of the given
// scope. Errors if scope=user is requested but no user is attached to ctx.
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

// readableRoots returns the roots to walk for search/list. "both" includes
// shared and, if a user is attached, that user's tree.
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

// joinScope joins the scope root with a caller-supplied relative path.
func joinScope(root, path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return root
	}
	return root + "/" + path
}

func readHandler(fs *memory.FS) tools.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path  string `json:"path"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		root, err := writableRoot(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		return fs.Read(joinScope(root.Dir, in.Path))
	}
}

func writeHandler(fs *memory.FS) tools.Handler {
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
		full := joinScope(root.Dir, in.Path)
		if err := fs.Write(full, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s:%s (%d bytes)", root.Label, in.Path, len(in.Content)), nil
	}
}

func appendHandler(fs *memory.FS) tools.Handler {
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
		full := joinScope(root.Dir, in.Path)
		if err := fs.Append(full, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("appended to %s:%s (%d bytes)", root.Label, in.Path, len(in.Content)), nil
	}
}

func searchHandler(fs *memory.FS) tools.Handler {
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
				fmt.Fprintf(&sb, "%s:%s:%d  %s\n", r.Label, rel, h.Line, h.Snippet)
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

func listHandler(fs *memory.FS) tools.Handler {
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
			start := joinScope(r.Dir, in.Dir)
			files, err := fs.List(start)
			if err != nil {
				continue
			}
			for _, f := range files {
				rel := strings.TrimPrefix(f, r.Dir+"/")
				fmt.Fprintf(&sb, "%s:%s\n", r.Label, rel)
			}
		}
		out := strings.TrimRight(sb.String(), "\n")
		if out == "" {
			return "(empty)", nil
		}
		return out, nil
	}
}
