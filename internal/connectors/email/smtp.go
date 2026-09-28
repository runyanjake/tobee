package email

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// smtpSend returns the Message-ID without angle brackets, as IMAP envelopes report it.
func (m *Mailbox) smtpSend(to, subject, body, inReplyTo string) (string, error) {
	host, port, err := net.SplitHostPort(m.cfg.SMTPAddr)
	if err != nil {
		return "", fmt.Errorf("email: smtp address: %w", err)
	}
	id := newMessageID(m.cfg.From)
	msg := compose(m.cfg.From, to, subject, body, id, inReplyTo, time.Now())
	auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, host)

	if port != "465" {
		// smtp.SendMail upgrades with STARTTLS when the server offers it
		// and refuses PLAIN auth over an unencrypted connection.
		if err := smtp.SendMail(m.cfg.SMTPAddr, auth, m.cfg.From, []string{to}, msg); err != nil {
			return "", fmt.Errorf("email: smtp send: %w", err)
		}
		return id, nil
	}

	conn, err := tls.Dial("tcp", m.cfg.SMTPAddr, &tls.Config{ServerName: host})
	if err != nil {
		return "", fmt.Errorf("email: smtp dial: %w", err)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return "", fmt.Errorf("email: smtp client: %w", err)
	}
	defer c.Close()
	if err := c.Auth(auth); err != nil {
		return "", fmt.Errorf("email: smtp auth: %w", err)
	}
	if err := c.Mail(m.cfg.From); err != nil {
		return "", fmt.Errorf("email: smtp MAIL: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return "", fmt.Errorf("email: smtp RCPT: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return "", fmt.Errorf("email: smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return "", fmt.Errorf("email: smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("email: smtp close: %w", err)
	}
	return id, c.Quit()
}

func compose(from, to, subject, body, id, inReplyTo string, now time.Time) []byte {
	var sb strings.Builder
	header := func(k, v string) { fmt.Fprintf(&sb, "%s: %s\r\n", k, v) }
	header("From", from)
	header("To", to)
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", "<"+id+">")
	if inReplyTo != "" {
		header("In-Reply-To", "<"+inReplyTo+">")
		header("References", "<"+inReplyTo+">")
	}
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "8bit")
	sb.WriteString("\r\n")
	sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	sb.WriteString("\r\n")
	return []byte(sb.String())
}

func newMessageID(from string) string {
	domain := "tobee.local"
	if at := strings.LastIndex(from, "@"); at >= 0 && at < len(from)-1 {
		domain = strings.Trim(from[at+1:], "> ")
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + "@" + domain
}
