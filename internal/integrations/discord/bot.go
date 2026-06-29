// Package discord is the Discord integration. It connects to the Discord
// gateway, pushes inbound messages onto the agent bus, and registers a
// reply sender so the agent can deliver responses back to the originating
// channel.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/runyanjake/tobee/internal/agent"
	"github.com/runyanjake/tobee/internal/integrations"
)

// Config configures a Bot.
type Config struct {
	Token     string
	ChannelID string // optional; if set only this channel is handled
}

// Bot is a Discord integration. It implements integrations.Integration.
type Bot struct {
	session   *discordgo.Session
	bus       *integrations.Bus
	channelID string

	namesMu sync.RWMutex
	names   map[string]string // user ID → display name

	statsMu sync.RWMutex
	rxLog   []rxEvent
	rxHead  int
	rxFill  bool
	lastRx  time.Time
	connect time.Time // set in onReady
}

type rxEvent struct {
	At time.Time
	Ch string
}

const rxRingSize = 32

// New creates a Bot and registers its reply sender. The websocket connection
// is not opened until Start is called.
func New(cfg Config, bus *integrations.Bus, replies *agent.Replies) (*Bot, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("discord token is empty")
	}
	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsAllWithoutPrivileged

	b := &Bot{
		session:   session,
		bus:       bus,
		channelID: cfg.ChannelID,
		names:     make(map[string]string),
		rxLog:     make([]rxEvent, rxRingSize),
	}
	session.AddHandler(b.onReady)
	session.AddHandler(b.onMessageCreate)

	replies.Register("discord", b.sendReply)
	return b, nil
}

func (b *Bot) Name() string { return "discord" }

// Start opens the gateway connection.
func (b *Bot) Start(_ context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open discord: %w", err)
	}
	slog.Info("discord: connected")
	return nil
}

// Stop closes the gateway connection.
func (b *Bot) Stop() error { return b.session.Close() }

func (b *Bot) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	b.statsMu.Lock()
	b.connect = time.Now()
	b.statsMu.Unlock()
	b.remember(r.User) // cache self so <@selfID> rewrites without waiting for a third party to mention us first.
	slog.Info("discord: ready", "user", r.User.Username, "guilds", len(r.Guilds))
	if b.channelID != "" {
		slog.Info("discord: listening on channel", "id", b.channelID)
	} else {
		slog.Info("discord: listening on all channels")
	}
}

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.Author.ID == s.State.User.ID {
		return
	}
	if b.channelID != "" && m.ChannelID != b.channelID {
		return
	}

	b.remember(m.Author)
	for _, u := range m.Mentions {
		b.remember(u)
	}

	if !b.isAddressed(s, m) {
		slog.Debug("discord: ambient (not addressed); dropping",
			"channel", m.ChannelID, "author", displayName(m.Author))
		return
	}

	content := strings.TrimSpace(b.rewriteMentions(m.Content))
	if content == "" {
		return
	}

	authorName := displayName(m.Author)
	b.recordRx(m.ChannelID)
	slog.Debug("discord: message received",
		"channel", m.ChannelID, "author", authorName,
		"author_id", m.Author.ID, "is_dm", m.GuildID == "",
		"content", content)

	b.bus.Publish(integrations.Envelope{
		Integration: "discord",
		User:        m.Author.ID,
		UserName:    authorName,
		Channel:     m.ChannelID,
		Content:     content,
		Received:    time.Now(),
		IsDirect:    m.GuildID == "",
	})
}

// nameRe matches the bot's name as a whole word, case-insensitive. Used to
// detect bare-name addressing ("tobee, are you there?") alongside the @-mention
// and reply-to-bot paths in isAddressed.
var nameRe = regexp.MustCompile(`(?i)\btobee\b`)

// isAddressed reports whether m is for the bot. We forward to the agent loop
// when any of: the bot is in the mentions list, the message is a reply to one
// of the bot's own messages, or the bot's name appears as a whole word in the
// body. Everything else is ambient channel chatter and gets dropped here so we
// don't burn an LLM call on it.
func (b *Bot) isAddressed(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	selfID := s.State.User.ID
	for _, u := range m.Mentions {
		if u != nil && u.ID == selfID {
			return true
		}
	}
	if ref := m.ReferencedMessage; ref != nil && ref.Author != nil && ref.Author.ID == selfID {
		return true
	}
	return nameRe.MatchString(m.Content)
}

// sendReply is registered on the agent's Replies table. channel is the
// Discord channel ID; thread is unused for now (future: forum/thread support).
func (b *Bot) sendReply(_ context.Context, channel, _, text string) error {
	text = b.rewriteOutboundMentions(text)
	chunks := splitMessage(text)
	for i, chunk := range chunks {
		slog.Debug("discord: message sent",
			"channel", channel, "chunk", i+1, "chunks", len(chunks),
			"chars", len(chunk), "content", chunk)
		if _, err := b.session.ChannelMessageSend(channel, chunk); err != nil {
			return fmt.Errorf("send: %w", err)
		}
	}
	return nil
}

func (b *Bot) recordRx(ch string) {
	now := time.Now()
	b.statsMu.Lock()
	defer b.statsMu.Unlock()
	b.rxLog[b.rxHead] = rxEvent{At: now, Ch: ch}
	b.rxHead = (b.rxHead + 1) % len(b.rxLog)
	if b.rxHead == 0 {
		b.rxFill = true
	}
	b.lastRx = now
}

// remember records a user ID → display-name mapping for later rewrites.
func (b *Bot) remember(u *discordgo.User) {
	if u == nil || u.ID == "" {
		return
	}
	name := displayName(u)
	if name == "" {
		return
	}
	b.namesMu.Lock()
	b.names[u.ID] = name
	b.namesMu.Unlock()
}

// mentionRe matches Discord's user-mention tokens. The optional `!` is the
// legacy nickname form; modern clients emit the plain form for both. Role
// (`<@&id>`) and channel (`<#id>`) tokens deliberately don't match.
var mentionRe = regexp.MustCompile(`<@!?(\d+)>`)

// rewriteMentions turns each `<@id>` in s into `@displayname` if the ID is
// in the cache; otherwise the token is left untouched so the raw ID survives.
func (b *Bot) rewriteMentions(s string) string {
	if s == "" || !strings.Contains(s, "<@") {
		return s
	}
	return mentionRe.ReplaceAllStringFunc(s, func(tok string) string {
		m := mentionRe.FindStringSubmatch(tok)
		if len(m) != 2 {
			return tok
		}
		b.namesMu.RLock()
		name, ok := b.names[m[1]]
		b.namesMu.RUnlock()
		if !ok {
			return tok
		}
		return "@" + name
	})
}

// rewriteOutboundMentions is the mirror of rewriteMentions: it turns
// `@displayname` tokens emitted by the model back into `<@id>` so Discord
// renders them as real pings. Longer names are matched first so a name that
// is a prefix of another can't steal the shorter match.
func (b *Bot) rewriteOutboundMentions(s string) string {
	if s == "" || !strings.Contains(s, "@") {
		return s
	}
	type entry struct{ id, name string }
	b.namesMu.RLock()
	entries := make([]entry, 0, len(b.names))
	for id, name := range b.names {
		if name != "" {
			entries = append(entries, entry{id, name})
		}
	}
	b.namesMu.RUnlock()
	if len(entries) == 0 {
		return s
	}
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].name) > len(entries[j].name)
	})
	for _, e := range entries {
		re, err := regexp.Compile(`@` + regexp.QuoteMeta(e.name) + `\b`)
		if err != nil {
			continue
		}
		s = re.ReplaceAllString(s, "<@"+e.id+">")
	}
	return s
}

// displayName prefers GlobalName (the new Discord display name) and falls
// back to the legacy Username. Returns "" if the user has neither.
func displayName(u *discordgo.User) string {
	if u == nil {
		return ""
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}
