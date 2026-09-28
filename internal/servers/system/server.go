// Package system is the built-in "system" MCP server: prompts/system/*.md as
// pinned resources, which the host puts in every system prompt in filename order (D-042).
package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/runyanjake/tobee/internal/mcpserver"
)

const uriPrefix = "system://prompt/"

// New pins each *.md file in dir. Files are read on every turn, so edits to
// bind-mounted prompts apply without a restart; new files need one.
func New(dir string) (*mcpserver.Server, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("system: glob %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("system: no prompt files in %s", dir)
	}
	sort.Strings(files)
	srv := mcpserver.New("system", "")
	for _, f := range files {
		name := filepath.Base(f)
		srv.AddResource(mcpserver.Resource{
			URI:      uriPrefix + name,
			Name:     strings.TrimSuffix(name, ".md"),
			MIMEType: "text/markdown",
			Pinned:   true,
			Read: func(context.Context) (string, error) {
				b, err := os.ReadFile(f)
				if err != nil {
					return "", err
				}
				return strings.TrimSpace(string(b)), nil
			},
		})
	}
	return srv, nil
}
