package openai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/runyanjake/tobee/internal/llm"
)

// decisionSchema admits {"call": {"tool": "<name>", "arguments": {...}}}; the root is
// an object, not a bare anyOf, because hosted APIs require one.
func decisionSchema(tools []llm.ToolSpec) (json.RawMessage, error) {
	branches := make([]any, 0, len(tools))
	for _, t := range tools {
		args, err := sanitizeTool(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("openai: schema for %s: %w", t.Name, err)
		}
		branches = append(branches, map[string]any{
			"type": "object",
			"properties": orderedObject{
				{"tool", map[string]any{"const": t.Name}},
				{"arguments", args},
			},
			"required":             []string{"tool", "arguments"},
			"additionalProperties": false,
		})
	}
	var call any = branches[0]
	if len(branches) > 1 {
		call = map[string]any{"anyOf": branches}
	}
	return json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"call": call},
		"required":             []string{"call"},
		"additionalProperties": false,
	})
}

// sanitizeTool keeps only keywords grammar converters handle: one unsupported
// keyword from an external server's schema can fail the whole request.
func sanitizeTool(raw json.RawMessage) (map[string]any, error) {
	var in map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
	}
	out := sanitize(in)
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out, nil
}

// Anything not kept, including $ref, relaxes to "any value".
var keep = map[string]bool{
	"type": true, "enum": true, "const": true, "required": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true,
}

func sanitize(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range s {
		switch {
		case keep[k]:
			out[k] = v
		case k == "properties":
			props, ok := v.(map[string]any)
			if !ok {
				continue
			}
			clean := make(map[string]any, len(props))
			for name, p := range props {
				if ps, ok := p.(map[string]any); ok {
					clean[name] = sanitize(ps)
				}
			}
			out[k] = clean
		case k == "items":
			if is, ok := v.(map[string]any); ok {
				out[k] = sanitize(is)
			}
		case k == "anyOf" || k == "oneOf":
			list, ok := v.([]any)
			if !ok {
				continue
			}
			clean := make([]any, 0, len(list))
			for _, b := range list {
				if bs, ok := b.(map[string]any); ok {
					clean = append(clean, sanitize(bs))
				}
			}
			out["anyOf"] = clean
		case k == "additionalProperties":
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		}
	}
	return out
}

// orderedObject keeps key order: grammar converters emit properties in schema order,
// and "tool" must precede "arguments" so the model names its choice first.
type orderedObject []struct {
	key string
	val any
}

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.key)
		v, err := json.Marshal(kv.val)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// renderMenu is protocol scaffolding, so it lives in code like the phase nudges.
func renderMenu(tools []llm.ToolSpec) string {
	var b strings.Builder
	b.WriteString("<tools>\nReply with one JSON object that calls exactly one of these tools:\n")
	b.WriteString(`{"call": {"tool": "<name>", "arguments": {...}}}` + "\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "\n%s", t.Name)
		if d := strings.TrimSpace(t.Description); d != "" {
			fmt.Fprintf(&b, ": %s", oneLine(d))
		}
		b.WriteByte('\n')
		var schema map[string]any
		_ = json.Unmarshal(t.InputSchema, &schema)
		for _, line := range argLines(schema) {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}
	b.WriteString("</tools>")
	return b.String()
}

// argLines renders `name (type, required): description`, required ones first.
func argLines(schema map[string]any) []string {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	required := map[string]bool{}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})

	lines := make([]string, 0, len(names))
	for _, n := range names {
		p, _ := props[n].(map[string]any)
		typ := typeString(p)
		if required[n] {
			typ += ", required"
		}
		line := fmt.Sprintf("%s (%s)", n, typ)
		if d, ok := p["description"].(string); ok && strings.TrimSpace(d) != "" {
			line += ": " + oneLine(d)
		}
		lines = append(lines, line)
	}
	return lines
}

func typeString(p map[string]any) string {
	if enum, ok := p["enum"].([]any); ok && len(enum) > 0 {
		parts := make([]string, len(enum))
		for i, e := range enum {
			b, _ := json.Marshal(e)
			parts[i] = string(b)
		}
		return strings.Join(parts, " | ")
	}
	switch t := p["type"].(type) {
	case string:
		switch t {
		case "array":
			if items, ok := p["items"].(map[string]any); ok {
				return "array of " + typeString(items)
			}
		case "object":
			if props, ok := p["properties"].(map[string]any); ok && len(props) > 0 {
				names := make([]string, 0, len(props))
				for n, v := range props {
					vs, _ := v.(map[string]any)
					names = append(names, n+": "+typeString(vs))
				}
				sort.Strings(names)
				return "{" + strings.Join(names, ", ") + "}"
			}
		}
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " | ")
	}
	return "any"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
