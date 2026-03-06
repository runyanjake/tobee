package ai

import (
	"bytes"
	"fmt"
	"os"
	"text/template"
)

// ChatTemplate is a parsed Go text/template used to render AI prompt messages.
// Templates are plain text/markdown files with {{.Field}} placeholders.
type ChatTemplate struct {
	tmpl *template.Template
}

// LoadTemplate reads a template file from disk and parses it.
func LoadTemplate(path string) (*ChatTemplate, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading template %s: %w", path, err)
	}
	return ParseTemplate(path, string(content))
}

// ParseTemplate parses a template from a string. name is used in error messages.
func ParseTemplate(name, content string) (*ChatTemplate, error) {
	tmpl, err := template.New(name).Parse(content)
	if err != nil {
		return nil, fmt.Errorf("parsing template %q: %w", name, err)
	}
	return &ChatTemplate{tmpl: tmpl}, nil
}

// Render executes the template with data and returns the rendered string.
// data can be any value — use ChatData for the standard chat template fields,
// or a map[string]any for ad-hoc templates.
func (t *ChatTemplate) Render(data any) (string, error) {
	var buf bytes.Buffer
	if err := t.tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering template: %w", err)
	}
	return buf.String(), nil
}

// ChatData is the standard data contract for prompts/chat.md.
// Extend this struct when new template fields are needed.
type ChatData struct {
	UserRequest string
	Tools       []ToolDef
	Memories    []string // relevant memories recalled for this request
}

// ToolDef describes an action exposed to the AI in the chat template.
type ToolDef struct {
	Name        string
	Description string
}
