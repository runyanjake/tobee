// Package workspace parses WORKSPACE_AREA_<NAME>[_DESC|_READONLY] env vars into
// host-directory areas, each a sandboxfs.FS so the model cannot escape it (D-003).
package workspace

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/runyanjake/tobee/internal/sandboxfs"
)

const (
	envPrefix     = "WORKSPACE_AREA_"
	suffixDesc    = "_DESC"
	suffixReadOnl = "_READONLY"
)

type Area struct {
	Name        string
	Description string
	ReadOnly    bool
	FS          *sandboxfs.FS
}

// AreaInfo omits the root path on purpose so discovery never leaks the host location.
type AreaInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"readonly,omitempty"`
}

type Areas struct {
	byName map[string]*Area
	order  []string // areas in declaration order, lowercased
}

func (a *Areas) Get(name string) (*Area, bool) {
	if a == nil {
		return nil, false
	}
	ar, ok := a.byName[strings.ToLower(name)]
	return ar, ok
}

// Len of zero means the workspace server should not be registered.
func (a *Areas) Len() int {
	if a == nil {
		return 0
	}
	return len(a.order)
}

func (a *Areas) List() []AreaInfo {
	if a == nil || len(a.order) == 0 {
		return nil
	}
	names := append([]string(nil), a.order...)
	sort.Strings(names)
	out := make([]AreaInfo, 0, len(names))
	for _, n := range names {
		ar := a.byName[n]
		out = append(out, AreaInfo{
			Name:        ar.Name,
			Description: ar.Description,
			ReadOnly:    ar.ReadOnly,
		})
	}
	return out
}

// LoadAreas skips entries with no root and reports orphans in the error
// alongside a usable registry.
func LoadAreas(env []string, maxFileSize int64) (*Areas, error) {
	type raw struct {
		root        string
		description string
		readonly    bool
		seenRoot    bool
		seenDesc    bool
		seenRO      bool
	}
	bySuffix := map[string]*raw{}
	order := []string{}

	get := func(suffix string) *raw {
		r, ok := bySuffix[suffix]
		if !ok {
			r = &raw{}
			bySuffix[suffix] = r
			order = append(order, strings.ToLower(suffix))
		}
		return r
	}

	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, envPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(key, envPrefix)
		if suffix == "" {
			continue
		}
		switch {
		case strings.HasSuffix(suffix, suffixDesc):
			name := strings.TrimSuffix(suffix, suffixDesc)
			if name == "" {
				continue
			}
			r := get(name)
			r.description = val
			r.seenDesc = true
		case strings.HasSuffix(suffix, suffixReadOnl):
			name := strings.TrimSuffix(suffix, suffixReadOnl)
			if name == "" {
				continue
			}
			r := get(name)
			r.readonly = isTruthy(val)
			r.seenRO = true
		default:
			r := get(suffix)
			r.root = val
			r.seenRoot = true
		}
	}

	a := &Areas{byName: map[string]*Area{}}
	var orphanErrs []string

	for _, name := range order {
		// order holds lowercase names; bySuffix keys are the env suffix, assumed uppercase.
		raw := bySuffix[strings.ToUpper(name)]
		if raw == nil {
			continue
		}
		if !raw.seenRoot {
			what := []string{}
			if raw.seenDesc {
				what = append(what, "_DESC")
			}
			if raw.seenRO {
				what = append(what, "_READONLY")
			}
			orphanErrs = append(orphanErrs, fmt.Sprintf("%s (%s set but no root)", name, strings.Join(what, "+")))
			continue
		}
		if strings.TrimSpace(raw.root) == "" {
			orphanErrs = append(orphanErrs, fmt.Sprintf("%s (empty root)", name))
			continue
		}
		fs, err := sandboxfs.NewFS(raw.root, maxFileSize)
		if err != nil {
			return nil, fmt.Errorf("workspace area %q: %w", name, err)
		}
		a.byName[name] = &Area{
			Name:        name,
			Description: raw.description,
			ReadOnly:    raw.readonly,
			FS:          fs,
		}
		a.order = append(a.order, name)
	}

	if len(orphanErrs) > 0 {
		return a, errors.New("workspace areas with no root: " + strings.Join(orphanErrs, ", "))
	}
	return a, nil
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}
