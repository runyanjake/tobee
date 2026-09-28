package email

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/runyanjake/tobee/internal/event"
)

// maxPerPoll bounds how many messages one poll admits, so a flooded inbox
// cannot fill the task queue in one go. The rest wait for the next poll.
const maxPerPoll = 20

// message is one fetched mail, reduced to what tobee uses.
type message struct {
	MessageID string
	InReplyTo string
	From      string
	FromName  string
	Subject   string
	Body      string
	Date      time.Time
}

func (msg message) event() event.Event {
	content := strings.TrimSpace(msg.Body)
	if msg.Subject != "" {
		content = "Subject: " + msg.Subject + "\n\n" + content
	}
	return event.Event{
		ID:        msg.MessageID,
		Source:    Name,
		Kind:      event.KindMessage,
		Actor:     event.Actor{ID: normalize(msg.From), Name: msg.FromName},
		Origin:    event.Address{Connector: Name, Channel: normalize(msg.From), Thread: msg.MessageID},
		InReplyTo: msg.InReplyTo,
		Content:   content,
		Received:  msg.Date,
	}
}

// poll fetches unseen mail from allowed senders and marks it seen. Mail
// from anyone else is left untouched: unread, and never parsed.
func (m *Mailbox) poll() ([]message, error) {
	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := c.Logout().Wait(); err != nil {
			slog.Debug("email: logout failed", "err", err)
		}
		_ = c.Close()
	}()

	if err := c.Login(m.cfg.Username, m.cfg.Password).Wait(); err != nil {
		return nil, fmt.Errorf("email: login: %w", err)
	}
	if _, err := c.Select(m.cfg.Mailbox, nil).Wait(); err != nil {
		return nil, fmt.Errorf("email: select %s: %w", m.cfg.Mailbox, err)
	}

	data, err := c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("email: search: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}

	// Envelopes first: the sender decides whether the body is read at all.
	envs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("email: fetch envelopes: %w", err)
	}
	var wanted []imap.UID
	for _, buf := range envs {
		if buf.Envelope == nil || len(buf.Envelope.From) == 0 {
			continue
		}
		if m.allowed[normalize(buf.Envelope.From[0].Addr())] {
			wanted = append(wanted, buf.UID)
		}
		if len(wanted) == maxPerPoll {
			break
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	section := &imap.FetchItemBodySection{Peek: true}
	bufs, err := c.Fetch(imap.UIDSetNum(wanted...), &imap.FetchOptions{
		UID:         true,
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("email: fetch bodies: %w", err)
	}

	out := make([]message, 0, len(bufs))
	var done []imap.UID
	for _, buf := range bufs {
		env := buf.Envelope
		if env == nil || len(env.From) == 0 {
			continue
		}
		body, err := plainText(buf.FindBodySection(section))
		if err != nil {
			slog.Warn("email: unreadable body; skipping", "uid", buf.UID, "err", err)
			continue
		}
		msg := message{
			MessageID: env.MessageID,
			From:      env.From[0].Addr(),
			FromName:  env.From[0].Name,
			Subject:   env.Subject,
			Body:      stripQuoted(body),
			Date:      env.Date,
		}
		if msg.MessageID == "" {
			msg.MessageID = fmt.Sprintf("uid-%d", buf.UID)
		}
		if len(env.InReplyTo) > 0 {
			msg.InReplyTo = env.InReplyTo[0]
		}
		out = append(out, msg)
		done = append(done, buf.UID)
	}

	if len(done) > 0 {
		store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
		if err := c.Store(imap.UIDSetNum(done...), store, nil).Close(); err != nil {
			// Dedup in the ingest engine absorbs the repeat on the next poll.
			slog.Warn("email: mark seen failed", "err", err)
		}
	}
	return out, nil
}

func (m *Mailbox) dial() (*imapclient.Client, error) {
	var (
		c   *imapclient.Client
		err error
	)
	if strings.HasSuffix(m.cfg.IMAPAddr, ":993") {
		c, err = imapclient.DialTLS(m.cfg.IMAPAddr, nil)
	} else {
		c, err = imapclient.DialStartTLS(m.cfg.IMAPAddr, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("email: dial %s: %w", m.cfg.IMAPAddr, err)
	}
	return c, nil
}
