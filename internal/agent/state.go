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

// StateData is the render context passed to prompts/state/*.md templates.
// Fields not relevant to a given template stay zero-valued; templates
// gate on them with `{{if .Step}}…{{end}}` etc.
type StateData struct {
	UserInput         string
	Plan              *Plan
	Step              *Step
	StepNumber        int // 1-indexed for display
	StepTotal         int
	AvailableTools    []string
	SurfacedKnowledge []string // stub for future web / file search integration
}

// StateTemplates loads prompts/state/*.md at boot and renders any
// requested template with a StateData payload. Failing to render is a
// programming error, not a runtime input problem — every template must
// parse at load, and every field a template references must exist on
// StateData.
type StateTemplates struct {
	dir   string
	tmpls map[string]*template.Template
}

// LoadStateTemplates parses every *.md file under dir. Returns a
// non-nil StateTemplates even if the directory is empty — Render will
// error on unknown names.
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

// Names returns the loaded template names (without the .md suffix).
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

// Render executes the named template with data and returns the
// resulting string. Returns an error if the name is not loaded or the
// template body references a field that would panic.
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
