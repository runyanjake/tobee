// Package resources is the built-in "resources" MCP server: the model's one
// read path to any server's MCP resources, routed by the host (D-042).
package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/mcpserver"
)

func New(instructions string, host *mcphost.Host) *mcpserver.Server {
	srv := mcpserver.New("resources", instructions)
	srv.Add(mcpserver.Tool{
		Name: "list",
		Description: "List the resources and resource templates every server offers. " +
			"Templates show the URI shape; fill them in and read with resources_read.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server": {"type": "string", "description": "Only this server's resources. Omit for all."}
			}
		}`),
		ReadOnly: true,
		Handler:  listHandler(host),
	})
	srv.Add(mcpserver.Tool{
		Name:        "read",
		Description: "Read a resource by URI, such as memory://user/INDEX.md or workspace://notes/todo.md.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"uri": {"type": "string", "description": "The resource URI."}
			},
			"required": ["uri"]
		}`),
		ReadOnly: true,
		Handler:  readHandler(host),
	})
	return srv
}

func listHandler(host *mcphost.Host) mcpserver.Handler {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Server string `json:"server"`
		}
		_ = json.Unmarshal(args, &in)
		var sb strings.Builder
		for _, r := range host.Resources() {
			// Pinned resources are already in the system prompt.
			if r.Pinned || (in.Server != "" && r.Server != in.Server) {
				continue
			}
			kind := "resource"
			if r.Template {
				kind = "template"
			}
			fmt.Fprintf(&sb, "%s  %s  %s", r.Server, kind, r.URI)
			if r.Description != "" {
				fmt.Fprintf(&sb, "  %s", strings.Join(strings.Fields(r.Description), " "))
			}
			sb.WriteByte('\n')
		}
		if sb.Len() == 0 {
			return "(no resources)", nil
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}
}

func readHandler(host *mcphost.Host) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if strings.TrimSpace(in.URI) == "" {
			return "", fmt.Errorf("uri is required")
		}
		return host.ReadResource(ctx, strings.TrimSpace(in.URI))
	}
}
