package email

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 charsets
	"github.com/emersion/go-message/mail"
)

// maxBodyBytes caps the text taken from one message.
const maxBodyBytes = 16 * 1024

// plainText returns the first text/plain part of a raw RFC 5322 message.
// HTML-only mail yields an error: tobee does not render HTML.
func plainText(raw []byte) (string, error) {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && r == nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("no text/plain part")
		}
		if err != nil && p == nil {
			return "", fmt.Errorf("next part: %w", err)
		}
		h, ok := p.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		ct, _, _ := h.ContentType()
		if ct != "" && ct != "text/plain" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p.Body, maxBodyBytes))
		if err != nil {
			return "", fmt.Errorf("read part: %w", err)
		}
		return string(b), nil
	}
}

// wroteRe matches the attribution line mail clients put above a quote:
// "On Mon, 1 Jan 2026 at 10:00, Name <a@b.c> wrote:".
var wroteRe = regexp.MustCompile(`(?i)^on .+ wrote:\s*$`)

// stripQuoted drops the quoted history under a reply. The model only
// needs the new text; the quote is the question tobee already has.
func stripQuoted(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if wroteRe.MatchString(t) || t == "-----Original Message-----" {
			break
		}
		if strings.HasPrefix(t, ">") {
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
