package mcphost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/ingest"
)

// ResourceSource turns an MCP server's resource subscriptions into
// ingest events: each resources/updated notification is read and emitted
// as a notification event answered at ReplyTo. This is how an MCP server
// feeds work to tobee rather than only answering tool calls.
type ResourceSource struct {
	host    *Host
	server  string
	uris    []string
	replyTo event.Address
}

// NewResourceSource watches uris on a connected server.
func NewResourceSource(h *Host, server string, uris []string, replyTo event.Address) *ResourceSource {
	return &ResourceSource{host: h, server: server, uris: uris, replyTo: replyTo}
}

// Name implements ingest.Source.
func (s *ResourceSource) Name() string { return "mcp_" + s.server }

// Run implements ingest.Source.
func (s *ResourceSource) Run(ctx context.Context, emit ingest.Emit) error {
	session, ok := s.host.Session(s.server)
	if !ok {
		return fmt.Errorf("mcp source: server %q not connected", s.server)
	}
	s.host.watch(s.server, func(uri string) { s.updated(ctx, session, uri, emit) })
	defer s.host.watch(s.server, nil)

	for _, uri := range s.uris {
		if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			return fmt.Errorf("mcp source: subscribe %s on %s: %w", uri, s.server, err)
		}
	}
	slog.Info("mcp source: subscribed", "server", s.server, "uris", len(s.uris))

	<-ctx.Done()

	uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, uri := range s.uris {
		_ = session.Unsubscribe(uctx, &mcp.UnsubscribeParams{URI: uri})
	}
	return nil
}

func (s *ResourceSource) updated(ctx context.Context, session *mcp.ClientSession, uri string, emit ingest.Emit) {
	rctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	res, err := session.ReadResource(rctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		slog.Warn("mcp source: read failed", "server", s.server, "uri", uri, "err", err)
		return
	}
	var parts []string
	for _, c := range res.Contents {
		if c != nil && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	body := truncate(strings.Join(parts, "\n"))
	// Same content twice is the same event; the engine dedups on ID.
	sum := sha256.Sum256([]byte(uri + "\x00" + body))
	emit(event.Event{
		ID:       hex.EncodeToString(sum[:12]),
		Source:   s.Name(),
		Kind:     event.KindNotification,
		Origin:   s.replyTo,
		Content:  fmt.Sprintf("[resource updated: %s on %s]\n%s", uri, s.server, body),
		Received: time.Now(),
	})
}
