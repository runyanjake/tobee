// Package memory is the built-in "memory" MCP server over the memory sandbox,
// scoped per call to the turn's user, shared, or both (search/list only).
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/sandboxfs"
	"github.com/runyanjake/tobee/internal/scope"
)

const (
	sharedRoot = "shared"
	uriPrefix  = "memory://"

	// Lessons are the one memory file put in every system prompt (D-052).
	// The caps are what make that safe: a bounded block, not a growing one.
	lessonsPath      = "lessons.md"
	lessonsMaxBytes  = 1024
	lessonsMaxPerRun = 3
	lessonLineMax    = 200

	// The manifest is names only, capped, so the model knows what it has
	// already saved without reading any of it (D-054).
	manifestURI       = uriPrefix + "user/.files"
	manifestMaxFiles  = 50
	manifestMaxBytes  = 1024
	conversationsRoot = "conversations"
)

// lessonsPreamble is framing, like the <servers> and <context> scaffolding:
// the model must not read its own past guesses as facts.
const lessonsPreamble = "What earlier turns learned by failing. Guidance, not fact — " +
	"check with a tool before relying on any of it."

func New(instructions string, fs *sandboxfs.FS) *mcpserver.Server {
	srv := mcpserver.New("memory", instructions)
	srv.AddResourceTemplate(mcpserver.ResourceTemplate{
		URITemplate: uriPrefix + "{scope}/{+path}",
		Name:        "memory",
		Description: `Memory files. scope is "user" (the current user's tree) or "shared". ` +
			`memory_list and memory_search return these URIs.`,
		MIMEType: "text/markdown",
		Read:     readResource(fs),
	})

	// Pinned: the model kept starting a second file for something it had
	// already saved, because knowing what exists needed a tool call it didn't
	// make. Names only, so this stays structural context like <servers> (D-054).
	srv.AddResource(mcpserver.Resource{
		URI:         manifestURI,
		Name:        "files",
		Description: "The paths already saved in the user's memory. Names only, no contents.",
		MIMEType:    "text/markdown",
		Pinned:      true,
		// Last of the pinned blocks: it changes whenever a file is saved, so
		// everything above it stays cacheable (D-017).
		PinPriority: 0.5,
		Read:        readManifest(fs),
	})

	// Pinned, so guidance that must always apply doesn't depend on the model
	// choosing to look for it (D-052). Read fresh each turn, per user.
	srv.AddResource(mcpserver.Resource{
		URI:         uriPrefix + "user/" + lessonsPath,
		Name:        "lessons",
		Description: "What earlier turns learned from failing. Editable like any memory file.",
		MIMEType:    "text/markdown",
		Pinned:      true,
		// After the system fragments, before the manifest: lessons change
		// rarely, the file list changes often.
		PinPriority: 0.6,
		Read:        readLessons(fs),
	})

	srv.Add(mcpserver.Tool{
		Name: "write",
		Description: `Create or overwrite a memory file at exactly the path given. ` +
			`scope "user" (default) is the current user's files; "shared" is visible to everyone.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Path within the scope, e.g. INDEX.md or topics/food.md."},
				"content": {"type": "string", "description": "Full file contents."},
				"scope":   {"type": "string", "enum": ["user", "shared"], "description": "Default \"user\"."}
			},
			"required": ["path", "content"]
		}`),
		Handler: writeHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name:        "append",
		Description: `Append to a memory file, creating it if needed. Prefer this over memory_write for lists and journals.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path":    {"type": "string", "description": "Path within the scope."},
				"content": {"type": "string", "description": "Text to append; include a leading newline if needed."},
				"scope":   {"type": "string", "enum": ["user", "shared"], "description": "Default \"user\"."}
			},
			"required": ["path", "content"]
		}`),
		Handler: appendHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name:        "delete",
		Description: `Delete memory files. The user is asked to confirm before anything is deleted.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"uris": {"type": "array", "minItems": 1, "items": {"type": "string"}, "description": "memory:// URIs of the files, as memory_list returns them."}
			},
			"required": ["uris"]
		}`),
		Destructive: true,
		Handler:     deleteHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name:        "search",
		Description: `Case-insensitive substring search. Returns "<uri>:<line>  <snippet>" rows; read a hit with resources_read. Default scope is "both" (user + shared).`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search term"},
				"limit": {"type": "integer", "description": "Max hits to return (default 20)"},
				"scope": {"type": "string", "enum": ["user", "shared", "both"], "description": "Default \"both\"."}
			},
			"required": ["query"]
		}`),
		ReadOnly: true,
		Handler:  searchHandler(fs),
	})

	srv.Add(mcpserver.Tool{
		Name:        "list",
		Description: `List memory files as memory:// URIs; read one with resources_read. Default scope is "both" (user + shared).`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"dir":   {"type": "string", "description": "Optional subdirectory relative to the scope root"},
				"scope": {"type": "string", "enum": ["user", "shared", "both"], "description": "Default \"both\"."}
			}
		}`),
		ReadOnly: true,
		Handler:  listHandler(fs),
	})
	return srv
}

type scopedRoot struct {
	Label string // "user" | "shared"
	Dir   string // FS-relative root for this scope
}

// writableRoot errors on scope=user with no user attached rather than writing somewhere generic.
func writableRoot(ctx context.Context, scopeArg string) (scopedRoot, error) {
	switch scopeArg {
	case "", "user":
		s, ok := scope.From(ctx)
		if !ok || !s.HasUser() {
			return scopedRoot{}, fmt.Errorf(`user scope unavailable on this turn; pass scope="shared" instead`)
		}
		return scopedRoot{Label: "user", Dir: s.Dir()}, nil
	case "shared":
		return scopedRoot{Label: "shared", Dir: sharedRoot}, nil
	default:
		return scopedRoot{}, fmt.Errorf(`invalid scope %q (expected "user" or "shared")`, scopeArg)
	}
}

// readableRoots: "both" is shared plus the user's tree when a user is attached.
func readableRoots(ctx context.Context, scopeArg string) ([]scopedRoot, error) {
	switch scopeArg {
	case "", "both":
		roots := []scopedRoot{{Label: "shared", Dir: sharedRoot}}
		if s, ok := scope.From(ctx); ok && s.HasUser() {
			roots = append(roots, scopedRoot{Label: "user", Dir: s.Dir()})
		}
		return roots, nil
	case "user", "shared":
		r, err := writableRoot(ctx, scopeArg)
		if err != nil {
			return nil, err
		}
		return []scopedRoot{r}, nil
	default:
		return nil, fmt.Errorf(`invalid scope %q (expected "user", "shared", or "both")`, scopeArg)
	}
}

// joinScope confines p to the scope root. sandboxfs only keeps paths inside
// the whole memory tree, so "../<other user>" must be stopped here (D-013).
func joinScope(root scopedRoot, p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return root.Dir, nil
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q leaves the %s scope", p, root.Label)
	}
	return root.Dir + "/" + c, nil
}

// uri renders a scope-relative path as memory://<scope>/<path>.
func uri(label, rel string) string { return uriPrefix + label + "/" + rel }

// resolveURI maps memory://<scope>/<path> to an FS path inside that scope.
func resolveURI(ctx context.Context, u string) (string, error) {
	label, rel, ok := strings.Cut(strings.TrimPrefix(u, uriPrefix), "/")
	if !strings.HasPrefix(u, uriPrefix) || !ok || rel == "" {
		return "", fmt.Errorf("%q is not memory://<scope>/<path>", u)
	}
	root, err := writableRoot(ctx, label)
	if err != nil {
		return "", err
	}
	return joinScope(root, rel)
}

func readResource(fs *sandboxfs.FS) func(context.Context, string) (string, error) {
	return func(ctx context.Context, u string) (string, error) {
		full, err := resolveURI(ctx, u)
		if err != nil {
			return "", err
		}
		return fs.Read(full)
	}
}

func writeHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		root, err := writableRoot(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		full, err := joinScope(root, in.Path)
		if err != nil {
			return "", err
		}
		// Creating a second home for something already saved is worse than a
		// refusal: the user ends up with two shopping lists and no error (D-054).
		if clash := clashingPath(fs, root, full); clash != "" {
			return "", fmt.Errorf("%s already holds this: write there instead, or delete it first",
				uri(root.Label, clash))
		}
		if err := fs.Write(full, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s (%d bytes)", uri(root.Label, strings.TrimPrefix(in.Path, "/")), len(in.Content)), nil
	}
}

func appendHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		root, err := writableRoot(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		full, err := joinScope(root, in.Path)
		if err != nil {
			return "", err
		}
		if err := fs.Append(full, in.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("appended to %s (%d bytes)", uri(root.Label, strings.TrimPrefix(in.Path, "/")), len(in.Content)), nil
	}
}

func searchHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		roots, err := readableRoots(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		var sb strings.Builder
		remaining := limit
		for _, r := range roots {
			if remaining <= 0 {
				break
			}
			hits, err := fs.SearchUnder(in.Query, remaining, r.Dir)
			if err != nil {
				continue // missing dirs are fine; the FS swallows ENOENT
			}
			for _, h := range hits {
				rel := strings.TrimPrefix(h.Path, r.Dir+"/")
				fmt.Fprintf(&sb, "%s:%d  %s\n", uri(r.Label, rel), h.Line, h.Snippet)
				remaining--
				if remaining <= 0 {
					break
				}
			}
		}
		out := strings.TrimRight(sb.String(), "\n")
		if out == "" {
			return "no matches", nil
		}
		return out, nil
	}
}

func listHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Dir   string `json:"dir"`
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(args, &in)
		roots, err := readableRoots(ctx, in.Scope)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, r := range roots {
			start, err := joinScope(r, in.Dir)
			if err != nil {
				return "", err
			}
			files, err := fs.List(start)
			if err != nil {
				continue
			}
			for _, f := range files {
				rel := strings.TrimPrefix(f, r.Dir+"/")
				fmt.Fprintf(&sb, "%s\n", uri(r.Label, rel))
			}
		}
		out := strings.TrimRight(sb.String(), "\n")
		if out == "" {
			return "(empty)", nil
		}
		return out, nil
	}
}

// deleteHandler deletes every file it can and reports each outcome, so the
// reply's action line matches the filesystem exactly.
func deleteHandler(fs *sandboxfs.FS) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			URIs []string `json:"uris"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		var deleted, failed []string
		for _, u := range in.URIs {
			full, err := resolveURI(ctx, u)
			if err == nil {
				err = fs.Delete(full)
			}
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s (%v)", u, err))
				continue
			}
			deleted = append(deleted, u)
		}
		var sb strings.Builder
		if len(deleted) > 0 {
			fmt.Fprintf(&sb, "deleted %s", strings.Join(deleted, ", "))
		}
		if len(failed) > 0 {
			if sb.Len() > 0 {
				sb.WriteString("; ")
			}
			fmt.Fprintf(&sb, "not deleted: %s", strings.Join(failed, ", "))
		}
		if len(deleted) == 0 {
			return "", fmt.Errorf("%s", sb.String())
		}
		return sb.String(), nil
	}
}

// LinkIdentities moves a newly linked person's memory from its old
// per-account folder (users/<connector>/<account>) to users/<person>, so
// linking accounts never strands what was already remembered (D-045). With
// several old folders, the first moves and the rest are left for a human to
// merge.
func LinkIdentities(fs *sandboxfs.FS, people map[string][]string) {
	for person, accounts := range people {
		target := "users/" + person
		if fs.DirExists(target) {
			continue
		}
		moved := false
		for _, acct := range accounts {
			conn, id, _ := strings.Cut(acct, ":")
			legacy := scope.UserScope{Connector: conn, User: id}.Dir()
			if !fs.DirExists(legacy) {
				continue
			}
			if moved {
				slog.Warn("memory: another account folder for this person; merge by hand",
					"person", person, "folder", legacy, "into", target)
				continue
			}
			if err := fs.Rename(legacy, target); err != nil {
				slog.Error("memory: link failed", "person", person, "from", legacy, "err", err)
				continue
			}
			slog.Info("memory: linked account folder to person", "person", person, "from", legacy, "to", target)
			moved = true
		}
	}
}

// ArchiveTranscript writes a closed session under the person's
// conversations/ folder, where memory_search and resources_read reach it.
func ArchiveTranscript(fs *sandboxfs.FS, person string, started time.Time, markdown string) error {
	dir := scope.UserScope{Person: person, User: person}.Dir()
	path := fmt.Sprintf("%s/conversations/%s.md", dir, started.UTC().Format("2006/01/02-1504"))
	for i := 2; fs.Exists(path); i++ {
		path = fmt.Sprintf("%s/conversations/%s-%d.md", dir, started.UTC().Format("2006/01/02-1504"), i)
	}
	return fs.Write(path, markdown)
}

// readLessons renders the pinned lessons block, or nothing at all: a turn with
// no user (a timer or a notification) has no lessons, and neither does a fresh
// install. Never an error — a missing file must not make the prompt partial.
func readLessons(fs *sandboxfs.FS) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		s, ok := scope.From(ctx)
		if !ok || !s.HasUser() {
			return "", nil
		}
		body, err := fs.Read(path.Join(s.Dir(), lessonsPath))
		if err != nil || strings.TrimSpace(body) == "" {
			return "", nil
		}
		return "<lessons>\n" + lessonsPreamble + "\n" + strings.TrimSpace(body) + "\n</lessons>", nil
	}
}

// AppendLessons adds dated lines to a person's lessons file and trims the
// oldest away, so the pinned block stays bounded whatever happens (D-052).
// Called from the session archive path, which has a person but no scope.
func AppendLessons(fs *sandboxfs.FS, person string, lessons []string, now time.Time) error {
	if person == "" || len(lessons) == 0 {
		return nil
	}
	dir := scope.UserScope{Person: person, User: person}.Dir()
	file := path.Join(dir, lessonsPath)
	body, _ := fs.Read(file) // a missing file just starts an empty one

	stamp := now.Format("2006-01-02")
	lines := splitLines(body)
	for i, l := range lessons {
		if i >= lessonsMaxPerRun {
			break
		}
		if l = strings.TrimSpace(strings.ReplaceAll(l, "\n", " ")); l == "" {
			continue
		}
		if len(l) > lessonLineMax {
			l = l[:lessonLineMax] + "…"
		}
		lines = append(lines, "- "+stamp+" "+l)
	}
	return fs.Write(file, trimToBytes(lines, lessonsMaxBytes))
}

func splitLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " \t"))
		}
	}
	return out
}

// trimToBytes keeps the newest lines that fit, so the block never outgrows its cap.
func trimToBytes(lines []string, max int) string {
	for {
		out := strings.Join(lines, "\n") + "\n"
		if len(out) <= max || len(lines) <= 1 {
			return out
		}
		lines = lines[1:]
	}
}

// manifestPreamble is framing, like the <servers> and <context> scaffolding.
const manifestPreamble = "Files you have already saved for this user. " +
	"Before writing, check whether one of these is already the place for it — " +
	"read it and append rather than starting a second file for the same thing."

// readManifest lists the user's memory paths, names only. Transcripts are
// counted rather than listed: one per closed conversation would crowd out
// everything worth seeing. Never an error — no user or no files means nothing
// is pinned (D-054).
func readManifest(fs *sandboxfs.FS) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		s, ok := scope.From(ctx)
		if !ok || !s.HasUser() {
			return "", nil
		}
		dir := s.Dir()
		all, err := fs.List(dir)
		if err != nil {
			return "", nil
		}
		var files []string
		transcripts := 0
		for _, f := range all {
			rel := strings.TrimPrefix(strings.TrimPrefix(f, dir), "/")
			switch {
			case rel == "" || rel == lessonsPath || strings.HasPrefix(rel, "."):
				continue // already pinned in full, or ours
			case strings.HasPrefix(rel, conversationsRoot+"/"):
				transcripts++
			default:
				files = append(files, rel)
			}
		}
		if len(files) == 0 && transcripts == 0 {
			return "", nil
		}
		if len(files) > manifestMaxFiles {
			files = append(files[:manifestMaxFiles], fmt.Sprintf("…and %d more", len(files)-manifestMaxFiles))
		}
		if transcripts > 0 {
			files = append(files, fmt.Sprintf("%s/ (%d saved conversation%s)",
				conversationsRoot, transcripts, plural(transcripts)))
		}
		body := trimToBytes(files, manifestMaxBytes)
		return "<memory-files>\n" + manifestPreamble + "\n" + strings.TrimRight(body, "\n") + "\n</memory-files>", nil
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clashingPath names an existing file that is plainly the same thing as full,
// or "" when the write is new ground. Only high-confidence cases count: an
// exact overwrite is fine, and so are genuinely different names.
//
// The case that prompted this: "shopping_list.md" existed and the model wrote
// "shopping_list/INDEX.md", leaving two lists (D-054).
func clashingPath(fs *sandboxfs.FS, root scopedRoot, full string) string {
	if fs.Exists(full) {
		return "" // overwriting the same path is the intended way to update
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(full, root.Dir), "/")
	if rel == "" {
		return ""
	}
	existing, err := fs.List(root.Dir)
	if err != nil {
		return ""
	}

	// A directory segment of the new path is already a file: "shopping_list/…"
	// under an existing "shopping_list.md".
	segments := strings.Split(path.Dir(rel), "/")
	for i := range segments {
		if segments[i] == "." || segments[i] == "" {
			continue
		}
		prefix := strings.Join(segments[:i+1], "/")
		if hit := matchStem(existing, root.Dir, prefix); hit != "" {
			return hit
		}
	}
	// A sibling with the same name in a different spelling: shopping-list.md,
	// ShoppingList.md, shopping_list.txt.
	return matchStem(existing, root.Dir, rel)
}

// matchStem finds an existing file whose path matches target once extensions,
// case, and word separators are ignored.
func matchStem(existing []string, dir, target string) string {
	want := normalizeStem(target)
	if want == "" {
		return ""
	}
	for _, f := range existing {
		rel := strings.TrimPrefix(strings.TrimPrefix(f, dir), "/")
		if rel == "" || rel == target {
			continue
		}
		if normalizeStem(rel) == want {
			return rel
		}
	}
	return ""
}

// normalizeStem drops the extension, lowercases, and removes separators, so
// "Shopping-List.md" and "shopping_list.txt" are the same name.
func normalizeStem(p string) string {
	p = strings.TrimSuffix(p, path.Ext(p))
	var b strings.Builder
	for _, r := range strings.ToLower(p) {
		switch r {
		case '-', '_', ' ', '.':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
