package telemetry

import (
	"context"
	"log/slog"
	"strings"
)

// markerMaxLen keeps the tag column narrow enough to scan.
const markerMaxLen = 24

// Handler puts a scannable [TAG] at the front of every message and adds
// cat=system to any record that sets no category (D-040, D-058).
type Handler struct {
	inner  slog.Handler
	cat    Category // from WithAttrs, for records that carry no "subsystem: event" message
	hasCat bool
}

func NewHandler(h slog.Handler) *Handler { return &Handler{inner: h} }

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	cat := h.cat
	if !h.hasCat {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == Key {
				found = true
				if s, ok := a.Value.Any().(string); ok {
					cat = Category(s)
				}
			}
			return !found
		})
		if !found {
			cat = System
			r.AddAttrs(slog.String(Key, string(System)))
		}
	}
	// The message keeps its original text: CI and anyone grepping for a known
	// line still match, and the tag only adds a column to read by.
	out := slog.NewRecord(r.Time, r.Level, "["+Marker(r.Message, cat)+"] "+r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(a)
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &Handler{inner: h.inner.WithAttrs(attrs), cat: h.cat, hasCat: h.hasCat}
	for _, a := range attrs {
		if a.Key == Key {
			next.hasCat = true
			if s, ok := a.Value.Any().(string); ok {
				next.cat = Category(s)
			}
		}
	}
	return next
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), cat: h.cat, hasCat: h.hasCat}
}

// Marker derives the tag from the message itself — "agent: tool call" becomes
// TOOL_CALL — so no call site passes one and a tag can never disagree with the
// text beside it. A message with no "subsystem: event" shape falls back to its
// category.
func Marker(msg string, cat Category) string {
	_, event, ok := strings.Cut(msg, ": ")
	if !ok || strings.TrimSpace(event) == "" {
		if cat == "" {
			cat = System
		}
		return strings.ToUpper(string(cat))
	}
	var words []string
	for _, w := range strings.Fields(event) {
		w = keepAlnum(w)
		if w == "" {
			continue
		}
		words = append(words, strings.ToUpper(w))
		if len(words) == 3 {
			break
		}
	}
	if len(words) == 0 {
		return strings.ToUpper(string(cat))
	}
	tag := strings.Join(words, "_")
	if len(tag) > markerMaxLen {
		tag = tag[:markerMaxLen]
	}
	return tag
}

func keepAlnum(w string) string {
	var b strings.Builder
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}
