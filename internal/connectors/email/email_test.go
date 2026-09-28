package email

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/scope"
)

func TestStripQuoted(t *testing.T) {
	body := "The blue one.\r\n\r\nOn Mon, 28 Sep 2026 at 10:00, tobee <tobee@example.com> wrote:\r\n> Which folder?\r\n"
	if got := stripQuoted(body); got != "The blue one." {
		t.Fatalf("stripQuoted() = %q", got)
	}
	if got := stripQuoted("> quoted\nnew text"); got != "new text" {
		t.Fatalf("stripQuoted() = %q", got)
	}
}

func TestPlainText(t *testing.T) {
	raw := "From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nhello there\r\n"
	got, err := plainText([]byte(raw))
	if err != nil {
		t.Fatalf("plainText: %v", err)
	}
	if strings.TrimSpace(got) != "hello there" {
		t.Fatalf("plainText() = %q", got)
	}
}

func TestComposeThreadsReplies(t *testing.T) {
	msg := string(compose("tobee@example.com", "me@example.com", "Re: plans", "line one\nline two",
		"abc@example.com", "orig@example.com", time.Unix(0, 0)))
	for _, want := range []string{
		"Message-ID: <abc@example.com>\r\n",
		"In-Reply-To: <orig@example.com>\r\n",
		"References: <orig@example.com>\r\n",
		"\r\n\r\nline one\r\nline two\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("compose() missing %q:\n%s", want, msg)
		}
	}
}

// Outbound mail is allowlisted in code, not by prompt (D-038).
func TestSendRejectsUnlistedRecipient(t *testing.T) {
	m, err := New(Config{
		IMAPAddr: "imap.example.com:993", SMTPAddr: "smtp.example.com:587",
		Username: "u", Password: "p", From: "tobee@example.com",
		Allowed: []string{"Me@Example.com"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = m.Send(context.Background(), event.Address{Connector: Name, Channel: "stranger@example.com"}, "hi")
	if err == nil || !strings.Contains(err.Error(), "not an allowed recipient") {
		t.Fatalf("Send() err = %v, want allowlist rejection", err)
	}
}

func TestMessageEvent(t *testing.T) {
	ev := message{
		MessageID: "m1@example.com", InReplyTo: "q1@example.com",
		From: "Me@Example.com", FromName: "Me", Subject: "plans", Body: "yes",
	}.event()
	if ev.Actor.ID != "me@example.com" || ev.Origin.Channel != "me@example.com" {
		t.Fatalf("addresses not normalized: %+v", ev)
	}
	if ev.Origin.Thread != "m1@example.com" || ev.InReplyTo != "q1@example.com" {
		t.Fatalf("threading ids wrong: %+v", ev)
	}
	if ev.Content != "Subject: plans\n\nyes" {
		t.Fatalf("Content = %q", ev.Content)
	}
}

func TestSendToolRefusesCurrentSender(t *testing.T) {
	m, err := New(Config{
		IMAPAddr: "imap.example.com:993", SMTPAddr: "smtp.example.com:587",
		Username: "u", Password: "p", From: "tobee@example.com",
		Allowed: []string{"me@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := mcphost.New()
	defer h.Close()
	if err := h.ConnectInProcess(context.Background(), m.Server("")); err != nil {
		t.Fatal(err)
	}
	ctx := scope.With(context.Background(), scope.UserScope{Connector: Name, Channel: "me@example.com", User: "me@example.com"})
	res, err := h.Call(ctx, "email_send", json.RawMessage(`{"to":"Me@Example.com","subject":"s","body":"b"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "call reply instead") {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
