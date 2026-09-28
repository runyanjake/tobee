package telemetry

import (
	"context"
	"log/slog"
)

// Handler wraps an slog.Handler and adds cat=system to any record that
// does not set a category itself.
type Handler struct {
	inner  slog.Handler
	hasCat bool // a category was attached with WithAttrs
}

// NewHandler wraps h.
func NewHandler(h slog.Handler) *Handler { return &Handler{inner: h} }

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if !h.hasCat {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			found = a.Key == Key
			return !found
		})
		if !found {
			r.AddAttrs(slog.String(Key, string(System)))
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	has := h.hasCat
	for _, a := range attrs {
		if a.Key == Key {
			has = true
		}
	}
	return &Handler{inner: h.inner.WithAttrs(attrs), hasCat: has}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), hasCat: h.hasCat}
}
