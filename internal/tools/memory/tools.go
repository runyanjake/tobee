// Package memory wires the memory.FS into the agent's tool registry as a
// pack of read/write/search/list/append tools.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tobee/internal/memory"
	"tobee/internal/tools"
)

// Register adds memory.* tools to reg, backed by fs.
func Register(reg *tools.Registry, fs *memory.FS) {
	reg.MustRegister(tools.Spec{
		Name:        "memory.read",
		Description: "Read a memory file relative to data/memory. Use after consulting INDEX.md or memory.list.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Relative path under data/memory, e.g. \"user.md\" or \"facts/deploy.md\""}
			},
			"required": ["path"]
		}`),
		Handler: readHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.write",
		Description: "Create or overwrite a memory file with the given content. Use for small, stable facts. Paths are relative to data/memory.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Relative path under data/memory"},
				"content": {"type": "string", "description": "Full file contents"}
			},
			"required": ["path", "content"]
		}`),
		Handler: writeHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.append",
		Description: "Append content to a memory file, creating it if needed. Prefer this over memory.write when adding to a list or journal.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Relative path under data/memory"},
				"content": {"type": "string", "description": "Text to append (include leading newline if needed)"}
			},
			"required": ["path", "content"]
		}`),
		Handler: appendHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.search",
		Description: "Case-insensitive substring search across all memory files. Returns path:line snippets.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search term"},
				"limit": {"type": "integer", "description": "Max hits to return (default 20)"}
			},
			"required": ["query"]
		}`),
		Handler: searchHandler(fs),
	})

	reg.MustRegister(tools.Spec{
		Name:        "memory.list",
		Description: "List memory files under an optional subdirectory. Pass an empty dir to list everything.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"dir": {"type": "string", "description": "Optional subdirectory relative to data/memory"}
			}
		}`),
		Handler: listHandler(fs),
	})
}

func readHandler(fs *memory.FS) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return fs.Read(in.Path)
	}
}

func writeHandler(fs *memory.FS) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if err := fs.Write(in.Path, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s (%d bytes)", in.Path, len(in.Content)), nil
	}
}

func appendHandler(fs *memory.FS) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if err := fs.Append(in.Path, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("appended to %s (%d bytes)", in.Path, len(in.Content)), nil
	}
}

func searchHandler(fs *memory.FS) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		hits, err := fs.Search(in.Query, in.Limit)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "no matches", nil
		}
		var sb strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&sb, "%s:%d  %s\n", h.Path, h.Line, h.Snippet)
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}
}

func listHandler(fs *memory.FS) tools.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Dir string `json:"dir"`
		}
		// dir is optional; ignore unmarshal errors on empty args
		_ = json.Unmarshal(args, &in)
		files, err := fs.List(in.Dir)
		if err != nil {
			return "", err
		}
		if len(files) == 0 {
			return "(empty)", nil
		}
		return strings.Join(files, "\n"), nil
	}
}
