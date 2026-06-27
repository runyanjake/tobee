// Package workspace wires the configured workspace areas (see
// internal/workspace) into the agent's tool registry as a pack of
// areas/list/read/write/search tools.
//
// Every operation takes an `area` argument naming one configured area.
// workspace.search additionally accepts area="all" (the default) to walk
// every configured area in one call.
//
// Writes to a read-only area return a clear error. The model already sees
// the readonly flag via workspace.areas and the prompt-injected areas list,
// so this should be a rare path.
package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/tools"
	"github.com/runyanjake/tobee/internal/workspace"
)

// Register adds workspace.* tools to reg, backed by areas. Callers should
// skip this call when areas.Len() == 0 — an empty pack would expose tools
// with no usable area name, which is worse than no pack at all.
func Register(reg *tools.Registry, areas *workspace.Areas) {
	reg.MustRegister(tools.Spec{
		Name: "workspace.areas",
		Description: `List the workspace areas tobee has access to. Each entry has a name, ` +
			`optional description, and a readonly flag. Use the name as the "area" argument to ` +
			`workspace.list / read / write / search.`,
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		Handler:     areasHandler(areas),
	})

	reg.MustRegister(tools.Spec{
		Name:        "workspace.list",
		Description: `List files in a workspace area. Returns "<area>:<path>" rows.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area": {"type": "string", "description": "Configured area name (see workspace.areas)."},
				"path": {"type": "string", "description": "Optional subdirectory relative to the area root."}
			},
			"required": ["area"]
		}`),
		Handler: listHandler(areas),
	})

	reg.MustRegister(tools.Spec{
		Name:        "workspace.read",
		Description: `Read a file from a workspace area. Path is relative to the area root.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area": {"type": "string", "description": "Configured area name."},
				"path": {"type": "string", "description": "File path relative to the area root."}
			},
			"required": ["area", "path"]
		}`),
		Handler: readHandler(areas),
	})

	reg.MustRegister(tools.Spec{
		Name: "workspace.write",
		Description: `Create or overwrite a file in a workspace area. Fails if the area is ` +
			`read-only. Path is relative to the area root.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area":    {"type": "string", "description": "Configured area name."},
				"path":    {"type": "string", "description": "File path relative to the area root."},
				"content": {"type": "string", "description": "Full file contents."}
			},
			"required": ["area", "path", "content"]
		}`),
		Handler: writeHandler(areas),
	})

	reg.MustRegister(tools.Spec{
		Name: "workspace.search",
		Description: `Case-insensitive substring search across one or all workspace areas. ` +
			`Returns "<area>:<path>:<line>  <snippet>" rows. Default area is "all".`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search term."},
				"area":  {"type": "string", "description": "Configured area name, or \"all\" (default) to walk every area."},
				"limit": {"type": "integer", "description": "Max hits to return (default 20)."}
			},
			"required": ["query"]
		}`),
		Handler: searchHandler(areas),
	})
}

func areasHandler(areas *workspace.Areas) tools.Handler {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		out := areas.List()
		if len(out) == 0 {
			return "[]", nil
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal areas: %w", err)
		}
		return string(raw), nil
	}
}

func listHandler(areas *workspace.Areas) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Area string `json:"area"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		ar, ok := areas.Get(in.Area)
		if !ok {
			return "", unknownAreaErr(in.Area, areas)
		}
		files, err := ar.FS.List(in.Path)
		if err != nil {
			return "", err
		}
		if len(files) == 0 {
			return "(empty)", nil
		}
		var sb strings.Builder
		for _, f := range files {
			fmt.Fprintf(&sb, "%s:%s\n", ar.Name, f)
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}
}

func readHandler(areas *workspace.Areas) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Area string `json:"area"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		ar, ok := areas.Get(in.Area)
		if !ok {
			return "", unknownAreaErr(in.Area, areas)
		}
		return ar.FS.Read(in.Path)
	}
}

func writeHandler(areas *workspace.Areas) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Area    string `json:"area"`
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		ar, ok := areas.Get(in.Area)
		if !ok {
			return "", unknownAreaErr(in.Area, areas)
		}
		if ar.ReadOnly {
			return "", fmt.Errorf("area %q is read-only", ar.Name)
		}
		if err := ar.FS.Write(in.Path, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s:%s (%d bytes)", ar.Name, in.Path, len(in.Content)), nil
	}
}

func searchHandler(areas *workspace.Areas) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Query string `json:"query"`
			Area  string `json:"area"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if in.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}

		targets := []*workspace.Area{}
		switch strings.ToLower(in.Area) {
		case "", "all":
			for _, info := range areas.List() {
				ar, _ := areas.Get(info.Name)
				targets = append(targets, ar)
			}
		default:
			ar, ok := areas.Get(in.Area)
			if !ok {
				return "", unknownAreaErr(in.Area, areas)
			}
			targets = []*workspace.Area{ar}
		}

		var sb strings.Builder
		remaining := limit
		for _, ar := range targets {
			if remaining <= 0 {
				break
			}
			hits, err := ar.FS.Search(in.Query, remaining)
			if err != nil {
				continue
			}
			for _, h := range hits {
				fmt.Fprintf(&sb, "%s:%s:%d  %s\n", ar.Name, h.Path, h.Line, h.Snippet)
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

func unknownAreaErr(name string, areas *workspace.Areas) error {
	known := make([]string, 0, areas.Len())
	for _, info := range areas.List() {
		known = append(known, info.Name)
	}
	if len(known) == 0 {
		return fmt.Errorf("no workspace areas configured")
	}
	return fmt.Errorf("unknown area %q (configured: %s)", name, strings.Join(known, ", "))
}
