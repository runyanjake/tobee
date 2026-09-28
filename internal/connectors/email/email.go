// Package email is the IMAP/SMTP connector. Mail admits third-party text, so
// both directions are allowlisted in code (D-038).
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/ingest"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/scope"
)

const Name = "email"

type Config struct {
	IMAPAddr string // host:port; port 993 dials implicit TLS, anything else STARTTLS
	SMTPAddr string // host:port; port 465 dials implicit TLS, anything else STARTTLS
	Username string
	Password string
	From     string   // address tobee sends as
	Allowed  []string // addresses admitted inbound and allowed outbound
	Mailbox  string   // IMAP folder to poll; default INBOX
	Interval time.Duration
}

type Mailbox struct {
	cfg     Config
	allowed map[string]bool

	mu       sync.Mutex
	subjects map[string]string // Message-ID → subject, for reply threading
	polls    int
	received int
	sent     int
	lastPoll time.Time
	lastErr  string
}

func New(cfg Config) (*Mailbox, error) {
	if cfg.IMAPAddr == "" || cfg.SMTPAddr == "" || cfg.Username == "" || cfg.Password == "" || cfg.From == "" {
		return nil, fmt.Errorf("email: IMAP address, SMTP address, username, password, and from address are required")
	}
	if len(cfg.Allowed) == 0 {
		return nil, fmt.Errorf("email: at least one allowed address is required")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	allowed := make(map[string]bool, len(cfg.Allowed))
	for _, a := range cfg.Allowed {
		allowed[normalize(a)] = true
	}
	return &Mailbox{cfg: cfg, allowed: allowed, subjects: make(map[string]string)}, nil
}

func (m *Mailbox) Name() string { return Name }

func (m *Mailbox) Allowed() []string {
	out := make([]string, 0, len(m.allowed))
	for a := range m.allowed {
		out = append(out, a)
	}
	return out
}

// Run retries a failed poll on the next tick instead of restarting the
// source: mail servers drop idle connections routinely.
func (m *Mailbox) Run(ctx context.Context, emit ingest.Emit) error {
	slog.Info("email: polling", "mailbox", m.cfg.Mailbox, "interval", m.cfg.Interval)
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	for {
		m.pollOnce(emit)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (m *Mailbox) pollOnce(emit ingest.Emit) {
	msgs, err := m.poll()
	m.mu.Lock()
	m.polls++
	m.lastPoll = time.Now()
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
	if err != nil {
		slog.Warn("email: poll failed", "err", err)
	}
	for _, msg := range msgs {
		m.mu.Lock()
		m.received++
		if msg.MessageID != "" {
			m.subjects[msg.MessageID] = msg.Subject
		}
		m.mu.Unlock()
		emit(msg.event())
	}
}

// Send returns the sent Message-ID so a reply to it can resume a parked task.
func (m *Mailbox) Send(_ context.Context, to event.Address, text string) (string, error) {
	m.mu.Lock()
	subject, ok := m.subjects[to.Thread]
	m.mu.Unlock()
	switch {
	case ok && !strings.HasPrefix(strings.ToLower(subject), "re:"):
		subject = "Re: " + subject
	case !ok:
		subject = "Message from tobee"
	}
	return m.send(to.Channel, subject, text, to.Thread)
}

func (m *Mailbox) send(to, subject, body, inReplyTo string) (string, error) {
	if !m.allowed[normalize(to)] {
		return "", fmt.Errorf("email: %s is not an allowed recipient", to)
	}
	id, err := m.smtpSend(to, subject, body, inReplyTo)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.sent++
	m.subjects[id] = subject
	m.mu.Unlock()
	slog.Info("email: sent", "to", to, "subject", subject, "message_id", id)
	return id, nil
}

func (m *Mailbox) Server(instructions string) *mcpserver.Server {
	srv := mcpserver.New(Name, instructions)
	srv.Add(mcpserver.Tool{
		Name: "send",
		Description: "Send an email to an allowed address. Your reply to the current conversation is " +
			"sent automatically; do not use this for it.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":      {"type": "string", "description": "Recipient address. Must be on the operator's allowlist."},
				"subject": {"type": "string", "description": "Subject line."},
				"body":    {"type": "string", "description": "Plain-text body."}
			},
			"required": ["to", "subject", "body"]
		}`),
		OpenWorld: true,
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				To      string `json:"to"`
				Subject string `json:"subject"`
				Body    string `json:"body"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("invalid args: %w", err)
			}
			// The reply to the current sender is sent in code (D-035).
			if sc, ok := scope.From(ctx); ok && sc.Connector == Name && sc.Channel == normalize(in.To) {
				return "", fmt.Errorf("that is the current sender; your reply is delivered automatically, so call reply instead")
			}
			id, err := m.send(strings.TrimSpace(in.To), strings.TrimSpace(in.Subject), in.Body, "")
			if err != nil {
				return "", err
			}
			return "sent " + id, nil
		},
	})
	return srv
}

func normalize(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}
