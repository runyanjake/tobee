// Package workspace exposes one or more host-filesystem "areas" the agent
// can list, read, search, and (when not read-only) write.
//
// Each area is configured via environment variables:
//
//	WORKSPACE_AREA_<NAME>          = /abs/path/to/dir   (required)
//	WORKSPACE_AREA_<NAME>_DESC     = human-readable purpose (optional)
//	WORKSPACE_AREA_<NAME>_READONLY = true | 1 | yes        (optional)
//
// The <NAME> suffix is lowercased to form the area's identifier (the string
// the model passes to workspace.* tools). Each area's filesystem is a
// sandboxfs.FS rooted at the configured path; the resolve() guard prevents
// the model from escaping that root via .., absolute paths, or volume
// prefixes — exactly the same safety story as long-term memory (D-003).
//
// LoadAreas does not create directories outside what sandboxfs.NewFS would
// create (which is the root itself via os.MkdirAll). Configure paths that
// already exist if you want to avoid that.
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

// Area is one configured slice of the host filesystem the agent may access.
type Area struct {
	Name        string
	Description string
	ReadOnly    bool
	FS          *sandboxfs.FS
}

// AreaInfo is the metadata projection used for discovery (workspace.areas
// tool, system-prompt injection). It deliberately omits FS / root paths
// so callers cannot accidentally leak the host-side location.
type AreaInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ReadOnly    bool   `json:"readonly,omitempty"`
}

// Areas is the registry of all configured workspace areas.
type Areas struct {
	byName map[string]*Area
	order  []string // areas in declaration order, lowercased
}

// Get returns the area with the given name and whether it exists.
func (a *Areas) Get(name string) (*Area, bool) {
	if a == nil {
		return nil, false
	}
	ar, ok := a.byName[strings.ToLower(name)]
	return ar, ok
}

// Len returns the number of configured areas. Zero means the workspace
// feature should be treated as disabled — the tool pack should not register.
func (a *Areas) Len() int {
	if a == nil {
		return 0
	}
	return len(a.order)
}

// List returns AreaInfo for every area, sorted by name.
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

// LoadAreas parses workspace area definitions from env (typically os.Environ())
// and builds a sandboxfs.FS per area with maxFileSize as the per-file cap.
//
// Entries without a corresponding root (e.g. _DESC for an undefined area)
// are skipped — the returned error reports them so the operator can fix the
// config. Areas whose root path is empty are likewise skipped.
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
		// order was populated with lowercase names; recover the matching key.
		// We keep both keys identical so the suffix lookup is just uppercase.
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
