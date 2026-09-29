// Package identity links connector accounts to one person, so sessions,
// memory, and parked questions follow a user across Discord, email, and
// whatever comes next (D-045).
package identity

import (
	"fmt"
	"sort"
	"strings"
)

const envPrefix = "IDENTITY_"

// Directory maps "<connector>:<account>" to a person name.
type Directory struct {
	people map[string]string   // account → person
	byName map[string][]string // person → accounts
}

// Load parses IDENTITY_<NAME>=<connector>:<account>,… from environ. <NAME>
// is lowercased to form the person's name.
func Load(environ []string) (*Directory, error) {
	d := &Directory{people: map[string]string{}, byName: map[string][]string{}}
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, envPrefix) {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(key, envPrefix))
		if name == "" || strings.ContainsAny(name, ":/") {
			return nil, fmt.Errorf("identity: bad name in %s", key)
		}
		for _, acct := range strings.Split(val, ",") {
			conn, id, ok := strings.Cut(strings.TrimSpace(acct), ":")
			if !ok || conn == "" || id == "" {
				return nil, fmt.Errorf("identity: %s: %q is not <connector>:<account>", key, acct)
			}
			k := account(conn, id)
			if prev, dup := d.people[k]; dup && prev != name {
				return nil, fmt.Errorf("identity: %s belongs to both %s and %s", k, prev, name)
			}
			d.people[k] = name
			d.byName[name] = append(d.byName[name], k)
		}
	}
	return d, nil
}

// Person names who owns a connector account. Unlinked accounts are their
// own person, "<connector>:<account>". Empty when there is no account.
func (d *Directory) Person(connector, accountID string) string {
	if accountID == "" {
		return ""
	}
	k := account(connector, accountID)
	if d != nil {
		if p, ok := d.people[k]; ok {
			return p
		}
	}
	return k
}

// People returns linked people and their accounts, sorted by name.
func (d *Directory) People() map[string][]string {
	out := make(map[string][]string, len(d.byName))
	for n, accts := range d.byName {
		a := append([]string(nil), accts...)
		sort.Strings(a)
		out[n] = a
	}
	return out
}

func account(connector, id string) string {
	return strings.ToLower(connector) + ":" + strings.ToLower(strings.TrimSpace(id))
}
