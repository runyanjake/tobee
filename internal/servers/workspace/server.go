// Package workspace is the built-in "workspace" MCP server over the configured
// workspace areas; search defaults to area="all".
package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/datedname"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/workspace"
)

// New appends the areas to instructions (D-019). Skip it when areas.Len() == 0:
// a server with no usable area is worse than none.
func New(instructions string, areas *workspace.Areas) *mcpserver.Server {
	srv := mcpserver.New("workspace", withAreas(instructions, areas))
	srv.Add(mcpserver.Tool{
		Name: "areas",
		Description: `List the workspace areas tobee has access to. Each entry has a name, ` +
			`optional description, and a readonly flag. Use the name as the "area" argument to ` +
			`workspace_list / read / write / search.`,
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		ReadOnly:    true,
		Handler:     areasHandler(areas),
	})

	srv.Add(mcpserver.Tool{
		Name:        "list",
		Description: `List files in a workspace area. Returns "<area>:<path>" rows.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area": {"type": "string", "description": "Configured area name (see workspace_areas)."},
				"path": {"type": "string", "description": "Optional subdirectory relative to the area root."}
			},
			"required": ["area"]
		}`),
		ReadOnly: true,
		Handler:  listHandler(areas),
	})

	srv.Add(mcpserver.Tool{
		Name:        "read",
		Description: `Read a file from a workspace area. Path is relative to the area root.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area": {"type": "string", "description": "Configured area name."},
				"path": {"type": "string", "description": "File path relative to the area root."}
			},
			"required": ["area", "path"]
		}`),
		ReadOnly: true,
		Handler:  readHandler(areas),
	})

	srv.Add(mcpserver.Tool{
		Name: "write",
		Description: `Create or overwrite a file in a workspace area. Pass the filename you ` +
			`want; the backend prepends today's date and kebab-cases the name. "My Notes.md" ` +
			`becomes "YYYY.MM.DD-my-notes.md". Subdirectories are preserved. Fails if the area ` +
			`is read-only.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area":    {"type": "string", "description": "Configured area name."},
				"path":    {"type": "string", "description": "Filename (with optional subdir) relative to the area root. Date prefix and kebab-case applied automatically. Do not add a date yourself."},
				"content": {"type": "string", "description": "Full file contents."}
			},
			"required": ["area", "path", "content"]
		}`),
		Handler: writeHandler(areas),
	})

	srv.Add(mcpserver.Tool{
		Name: "search",
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
		ReadOnly: true,
		Handler:  searchHandler(areas),
	})
	return srv
}

// withAreas never includes the host path.
func withAreas(instructions string, areas *workspace.Areas) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(instructions))
	sb.WriteString("\n\nConfigured areas:\n")
	for _, ar := range areas.List() {
		fmt.Fprintf(&sb, "- %s", ar.Name)
		if ar.ReadOnly {
			sb.WriteString(" (read-only)")
		}
		if ar.Description != "" {
			fmt.Fprintf(&sb, ": %s", ar.Description)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func areasHandler(areas *workspace.Areas) mcpserver.Handler {
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

func listHandler(areas *workspace.Areas) mcpserver.Handler {
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

func readHandler(areas *workspace.Areas) mcpserver.Handler {
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

func writeHandler(areas *workspace.Areas) mcpserver.Handler {
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
		dated, err := datedname.Apply(in.Path, time.Now())
		if err != nil {
			return "", err
		}
		if err := ar.FS.Write(dated, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s:%s (%d bytes)", ar.Name, dated, len(in.Content)), nil
	}
}

func searchHandler(areas *workspace.Areas) mcpserver.Handler {
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
