package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/scope"
)

func host(t *testing.T) *mcphost.Host {
	t.Helper()
	b, err := New(Config{Token: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h := mcphost.New()
	t.Cleanup(h.Close)
	if err := h.ConnectInProcess(context.Background(), b.Server("")); err != nil {
		t.Fatal(err)
	}
	return h
}

// Sending into the conversation being answered duplicates the reply, which
// is delivered in code (D-035). Refused before any network call.
func TestSendMessageRefusesCurrentConversation(t *testing.T) {
	h := host(t)
	ctx := scope.With(context.Background(), scope.UserScope{Connector: Name, Channel: "123", User: "u"})
	res, err := h.Call(ctx, "discord_send_message", json.RawMessage(`{"channel_id":"123","text":"hi"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "call reply instead") {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestSendMessageIsExternal(t *testing.T) {
	for _, tool := range host(t).Tools() {
		if tool.Name == "discord_send_message" && tool.Category != llm.CategoryExternal {
			t.Fatalf("category = %q", tool.Category)
		}
	}
}

func msg(content string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		GuildID:   "g1",
		ChannelID: "c1",
		Content:   content,
	}}
}

// The rules that need no gateway lookup (D-050). selfID is the bot's user ID.
func TestAddressedInMessage(t *testing.T) {
	const self = "botid"

	reply := msg("what about milk")
	reply.ReferencedMessage = &discordgo.Message{ID: "m0", Author: &discordgo.User{ID: self}}

	replyToSomeoneElse := msg("what about milk")
	replyToSomeoneElse.ReferencedMessage = &discordgo.Message{ID: "m0", Author: &discordgo.User{ID: "other"}}

	mentioned := msg("hey <@botid> hi")
	mentioned.Mentions = []*discordgo.User{{ID: self}}

	dm := msg("start a shopping list")
	dm.GuildID = ""

	tests := []struct {
		name string
		m    *discordgo.MessageCreate
		ok   bool
		why  string
	}{
		{"dm needs no mention", dm, true, "dm"},
		{"user mention", mentioned, true, "mention"},
		{"raw mention token only", msg("hey <@botid> hi"), true, "mention"},
		{"legacy nickname token", msg("hey <@!botid> hi"), true, "mention"},
		{"reply to the bot", reply, true, "reply"},
		{"bare name", msg("tobee, add milk"), true, "name"},
		{"plain-text at-name", msg("Hey @TOBEE please start a list"), true, "name"},
		{"reply to someone else", replyToSomeoneElse, false, ""},
		{"ambient chatter", msg("milk is expensive"), false, ""},
		{"name inside a word", msg("octobeer is great"), false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := addressedInMessage(tc.m, self)
			if ok != tc.ok || why != tc.why {
				t.Fatalf("addressedInMessage() = (%v, %q), want (%v, %q)", ok, why, tc.ok, tc.why)
			}
		})
	}
}

// "@TOBEE" resolves to the bot's managed role, which never lands in
// m.Mentions — the case that read as ambient before D-050.
func TestPingsRole(t *testing.T) {
	own := map[string]bool{"role-tobee": true}
	tests := []struct {
		name   string
		pinged []string
		want   bool
	}{
		{"the bot's own role", []string{"role-tobee"}, true},
		{"among other roles", []string{"role-mods", "role-tobee"}, true},
		{"someone else's role", []string{"role-mods"}, false},
		{"everyone is not addressing", []string{"g1"}, false},
		{"no roles pinged", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pingsRole(tc.pinged, "g1", own); got != tc.want {
				t.Fatalf("pingsRole(%v) = %v, want %v", tc.pinged, got, tc.want)
			}
		})
	}
}

// DISCORD_CHANNEL_ID scopes guild chatter only; a DM is always in scope.
func TestInScopeAllowsDMsWhenAChannelIsConfigured(t *testing.T) {
	b, err := New(Config{Token: "test", ChannelID: "c-allowed"})
	if err != nil {
		t.Fatal(err)
	}
	dm := msg("hi")
	dm.GuildID = ""
	dm.ChannelID = "dm-channel"
	if !b.inScope(b.session, dm) {
		t.Fatal("inScope(DM) = false, want true")
	}
	if !b.inScope(b.session, &discordgo.MessageCreate{Message: &discordgo.Message{GuildID: "g1", ChannelID: "c-allowed"}}) {
		t.Fatal("inScope(configured channel) = false, want true")
	}
}

// The ID we carry from an inbound message is the user snowflake, which is what
// <@id> needs; the display name would not ping anyone (D-054).
func TestMentionUsesTheUserID(t *testing.T) {
	b, err := New(Config{Token: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Mention("264301820258680834"); got != "<@264301820258680834>" {
		t.Fatalf("Mention() = %q", got)
	}
	if got := b.Mention(""); got != "" {
		t.Fatalf("Mention(\"\") = %q, want empty", got)
	}
	// The author ID an event carries is exactly this field.
	m := msg("hi")
	m.Author = &discordgo.User{ID: "264301820258680834", GlobalName: "jake"}
	if b.Mention(m.Author.ID) != "<@264301820258680834>" {
		t.Fatal("Mention() does not match the inbound author ID")
	}
}
