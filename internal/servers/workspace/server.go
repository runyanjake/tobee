// Package workspace is the built-in "workspace" MCP server over the configured
// workspace areas; search defaults to area="all".
package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/sandboxfs"
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
			`workspace_list, workspace_write, and workspace_grep.`,
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		ReadOnly:    true,
		Handler:     areasHandler(areas),
	})

	srv.Add(mcpserver.Tool{
		Name:        "list",
		Description: `List files in a workspace area as workspace:// URIs; read one with resources_read.`,
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

	srv.AddResourceTemplate(mcpserver.ResourceTemplate{
		URITemplate: uriPrefix + "{area}/{+path}",
		Name:        "workspace",
		Description: "Files in a workspace area. workspace_list and workspace_grep return these URIs.",
		Read:        readResource(areas),
	})

	srv.Add(mcpserver.Tool{
		Name:        "write",
		Description: `Create or overwrite a file in a workspace area at exactly the path given. Fails if the area is read-only.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"area":    {"type": "string", "description": "Configured area name."},
				"path":    {"type": "string", "description": "Path relative to the area root."},
				"content": {"type": "string", "description": "Full file contents."}
			},
			"required": ["area", "path", "content"]
		}`),
		Handler: writeHandler(areas),
	})

	srv.Add(mcpserver.Tool{
		Name: "grep",
		Description: `Search workspace files with grep. The pattern is literal text unless you set regexp.

Returns "<uri>:<line>  <text>" rows; read one with resources_read, optionally with a #L.. range.
loose=true lets a phrase match any spelling of it. count=true gives per-file match counts. context=N shows surrounding lines.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "What to look for. Literal text unless regexp is true."},
				"loose":   {"type": "boolean", "description": "Let spaces, hyphens and underscores stand for each other."},
				"regexp":  {"type": "boolean", "description": "Treat the pattern as an extended regular expression."},
				"word":    {"type": "boolean", "description": "Whole words only."},
				"case":    {"type": "boolean", "description": "Case-sensitive. Insensitive by default."},
				"count":   {"type": "boolean", "description": "One row per file with its match count, instead of lines."},
				"context": {"type": "integer", "description": "Lines either side of each match, 0-10."},
				"area":    {"type": "string", "description": "Configured area name, or \"all\" (default) to walk every area."},
				"limit":   {"type": "integer", "description": "Max rows (default 20)."}
			},
			"required": ["pattern"]
		}`),
		ReadOnly: true,
		Handler:  grepHandler(areas),
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
			fmt.Fprintf(&sb, "%s%s/%s\n", uriPrefix, ar.Name, f)
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}
}

const uriPrefix = "workspace://"

func readResource(areas *workspace.Areas) func(context.Context, string) (string, error) {
	return func(_ context.Context, u string) (string, error) {
		area, rel, ok := strings.Cut(strings.TrimPrefix(u, uriPrefix), "/")
		if !ok || rel == "" {
			return "", fmt.Errorf("%q is not workspace://<area>/<path>", u)
		}
		ar, ok := areas.Get(area)
		if !ok {
			return "", unknownAreaErr(area, areas)
		}
		return ar.FS.Read(rel)
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
		if err := ar.FS.Write(in.Path, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s%s/%s (%d bytes)", uriPrefix, ar.Name, strings.TrimPrefix(in.Path, "/"), len(in.Content)), nil
	}
}

func grepHandler(areas *workspace.Areas) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Pattern string `json:"pattern"`
			Loose   bool   `json:"loose"`
			Regexp  bool   `json:"regexp"`
			Word    bool   `json:"word"`
			Case    bool   `json:"case"`
			Count   bool   `json:"count"`
			Context int    `json:"context"`
			Area    string `json:"area"`
			Limit   int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if in.Pattern == "" {
			return "", fmt.Errorf("pattern is required")
		}
		if in.Context > 10 {
			in.Context = 10
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
			hits, err := ar.FS.Grep(ctx, sandboxfs.GrepOptions{
				Pattern: in.Pattern, Regexp: in.Regexp, Loose: in.Loose, Word: in.Word,
				Case: in.Case, Context: in.Context, Count: in.Count, Limit: remaining,
			})
			if err != nil {
				return "", err
			}
			for _, h := range hits {
				u := fmt.Sprintf("%s%s/%s", uriPrefix, ar.Name, h.Path)
				switch {
				case h.Count > 0:
					fmt.Fprintf(&sb, "%s  %d matches\n", u, h.Count)
				case h.Context:
					fmt.Fprintf(&sb, "  %d  %s\n", h.Line, h.Text)
					continue // a context line rides along with its match
				default:
					fmt.Fprintf(&sb, "%s:%d  %s\n", u, h.Line, strings.TrimSpace(h.Text))
				}
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
