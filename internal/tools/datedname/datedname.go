// Package datedname rewrites a model-supplied filename into a dated,
// kebab-cased path. The contract: the model passes the filename it wants
// (e.g. "My Notes.md" or "ideas/dream journal"), the backend stamps the
// creation date and normalizes the basename so files land as
// "<dir>/YYYY.MM.DD-kebab-name.ext".
package datedname

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// Apply rewrites rel into "<dir>/YYYY.MM.DD-<kebab>.<ext>".
// Subdirectories are preserved; only the basename is rewritten.
func Apply(rel string, now time.Time) (string, error) {
	rel = strings.TrimSpace(rel)
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return "", fmt.Errorf("path must not be empty")
	}

	dir, base := path.Split(rel)
	if base == "" {
		return "", fmt.Errorf("path must include a filename")
	}

	ext := strings.ToLower(path.Ext(base))
	stem := strings.TrimSuffix(base, path.Ext(base))
	kebab := kebab(stem)
	if kebab == "" {
		return "", fmt.Errorf("filename %q has no usable characters", base)
	}

	dated := now.Format("2006.01.02") + "-" + kebab + ext
	if dir == "" {
		return dated, nil
	}
	return strings.TrimRight(dir, "/") + "/" + dated, nil
}

func kebab(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevHyphen := true
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevHyphen = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
