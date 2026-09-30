package agent

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// StateData is the render context for prompts/state/*.md. Kind is the event's
// kind, so the one turn directive can say what a timer means without a second
// phase (D-043, D-053).
type StateData struct {
	Kind string
}

// StateTemplates are parsed at boot; a render failure is a programming error, not bad input.
type StateTemplates struct {
	dir   string
	tmpls map[string]*template.Template
}

// LoadStateTemplates returns non-nil even for an empty dir; Render errors on unknown names.
func LoadStateTemplates(dir string) (*StateTemplates, error) {
	st := &StateTemplates{dir: dir, tmpls: make(map[string]*template.Template)}
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return st, fmt.Errorf("state: glob %q: %w", dir, err)
	}
	sort.Strings(matches)
	funcs := template.FuncMap{
		"join": strings.Join,
	}
	for _, p := range matches {
		body, err := os.ReadFile(p)
		if err != nil {
			return st, fmt.Errorf("state: read %q: %w", p, err)
		}
		name := strings.TrimSuffix(filepath.Base(p), ".md")
		tmpl, err := template.New(name).Funcs(funcs).Parse(string(body))
		if err != nil {
			return st, fmt.Errorf("state: parse %q: %w", p, err)
		}
		st.tmpls[name] = tmpl
	}
	slog.Info("state: templates loaded", "dir", dir, "count", len(st.tmpls))
	return st, nil
}

func (s *StateTemplates) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.tmpls))
	for n := range s.tmpls {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *StateTemplates) Render(name string, data StateData) (string, error) {
	if s == nil {
		return "", fmt.Errorf("state: not configured")
	}
	tmpl, ok := s.tmpls[name]
	if !ok {
		return "", fmt.Errorf("state: template %q not loaded (have: %v)", name, s.Names())
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("state: render %q: %w", name, err)
	}
	return buf.String(), nil
}

// RenderPhase wraps a template in <phase>, the boundary between harness directives and user text (D-029).
func (s *StateTemplates) RenderPhase(name string, data StateData) (string, error) {
	body, err := s.Render(name, data)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<phase name=%q>\n%s\n</phase>", name, strings.TrimSpace(body)), nil
}
