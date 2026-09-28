// Package discord is the Discord connector: an ingest source, a delivery
// channel (with edits and reactions), and an MCP server, all in one Bot (D-035).
package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/ingest"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/scope"
)

// Name is shared by the event source, the delivery channel, and the MCP server.
const Name = "discord"

type Config struct {
	Token     string
	ChannelID string // optional; restricts handling to this channel
}

type Bot struct {
	session   *discordgo.Session
	channelID string

	emitMu sync.RWMutex
	emit   ingest.Emit // nil while the source is not running

	namesMu sync.RWMutex
	names   map[string]string // user ID → display name

	statsMu sync.RWMutex
	rxLog   []rxEvent
	rxHead  int
	rxFill  bool
	lastRx  time.Time
	connect time.Time
}

type rxEvent struct {
	At time.Time
	Ch string
}

const rxRingSize = 32

func New(cfg Config) (*Bot, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("discord token is empty")
	}
	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	// Privileged intent (also enable it in the developer portal): without it guild
	// m.Content is empty unless the bot is @-mentioned, breaking bare-name addressing.
	session.Identify.Intents = discordgo.IntentsAllWithoutPrivileged | discordgo.IntentMessageContent

	b := &Bot{
		session:   session,
		channelID: cfg.ChannelID,
		names:     make(map[string]string),
		rxLog:     make([]rxEvent, rxRingSize),
	}
	session.AddHandler(b.onReady)
	session.AddHandler(b.onMessageCreate)
	return b, nil
}

func (b *Bot) Name() string { return Name }

func (b *Bot) Run(ctx context.Context, emit ingest.Emit) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord: open gateway: %w", err)
	}
	b.emitMu.Lock()
	b.emit = emit
	b.emitMu.Unlock()
	slog.Info("discord: connected")

	<-ctx.Done()

	b.emitMu.Lock()
	b.emit = nil
	b.emitMu.Unlock()
	b.statsMu.Lock()
	b.connect = time.Time{}
	b.statsMu.Unlock()
	if err := b.session.Close(); err != nil {
		slog.Warn("discord: close failed", "err", err)
	}
	slog.Info("discord: disconnected")
	return nil
}

func (b *Bot) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	b.statsMu.Lock()
	b.connect = time.Now()
	b.statsMu.Unlock()
	b.remember(r.User) // so <@selfID> rewrites before anyone else mentions the bot
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
		mentionIDs := make([]string, 0, len(m.Mentions))
		for _, u := range m.Mentions {
			if u != nil {
				mentionIDs = append(mentionIDs, u.ID)
			}
		}
		slog.Debug("discord: ambient (not addressed); dropping",
			"channel", m.ChannelID, "author", displayName(m.Author),
			"self_id", s.State.User.ID, "is_dm", m.GuildID == "",
			"mentions", mentionIDs, "content", m.Content)
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

	b.emitMu.RLock()
	emit := b.emit
	b.emitMu.RUnlock()
	if emit == nil {
		return
	}
	ev := event.Event{
		ID:        m.ID,
		Source:    Name,
		Kind:      event.KindMessage,
		Actor:     event.Actor{ID: m.Author.ID, Name: authorName},
		Origin:    event.Address{Connector: Name, Channel: m.ChannelID},
		MessageID: m.ID,
		Content:   content,
		Received:  time.Now(),
	}
	if ref := m.ReferencedMessage; ref != nil && ref.Author != nil && ref.Author.ID == s.State.User.ID {
		ev.InReplyTo = ref.ID
	}
	emit(ev)
}

// nameRe detects bare-name addressing; \b also matches plain-text "@TOBEE"
// because @ is a non-word character.
var nameRe = regexp.MustCompile(`(?i)\btobee\b`)

// isAddressed filters out ambient chatter. The raw <@id> check covers mentions
// missing from m.Mentions due to gateway state races.
func (b *Bot) isAddressed(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	selfID := s.State.User.ID
	for _, u := range m.Mentions {
		if u != nil && u.ID == selfID {
			return true
		}
	}
	if selfID != "" && (strings.Contains(m.Content, "<@"+selfID+">") || strings.Contains(m.Content, "<@!"+selfID+">")) {
		return true
	}
	if ref := m.ReferencedMessage; ref != nil && ref.Author != nil && ref.Author.ID == selfID {
		return true
	}
	return nameRe.MatchString(m.Content)
}

// Send returns the last chunk's ID; edits only make sense for one-chunk
// messages like the plan announcement.
func (b *Bot) Send(_ context.Context, to event.Address, text string) (string, error) {
	text = b.rewriteOutboundMentions(text)
	chunks := splitMessage(text)
	var lastID string
	for i, chunk := range chunks {
		slog.Debug("discord: message sent",
			"channel", to.Channel, "chunk", i+1, "chunks", len(chunks),
			"chars", len(chunk), "content", chunk)
		msg, err := b.session.ChannelMessageSend(to.Channel, chunk)
		if err != nil {
			return lastID, fmt.Errorf("send: %w", err)
		}
		if msg != nil {
			lastID = msg.ID
		}
	}
	return lastID, nil
}

func (b *Bot) Edit(_ context.Context, to event.Address, messageID, text string) error {
	text = b.rewriteOutboundMentions(text)
	slog.Debug("discord: message edited",
		"channel", to.Channel, "message_id", messageID,
		"chars", len(text), "content", text)
	if _, err := b.session.ChannelMessageEdit(to.Channel, messageID, text); err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	return nil
}

// React ignores ctx: discordgo's REST calls don't take one. Removing the
// bot's own reaction needs no Manage Messages permission.
func (b *Bot) React(_ context.Context, to event.Address, messageID, emoji string, add bool) error {
	if messageID == "" {
		return nil
	}
	if add {
		if err := b.session.MessageReactionAdd(to.Channel, messageID, emoji); err != nil {
			return fmt.Errorf("react add: %w", err)
		}
		return nil
	}
	if err := b.session.MessageReactionRemove(to.Channel, messageID, emoji, "@me"); err != nil {
		return fmt.Errorf("react remove: %w", err)
	}
	return nil
}

// Server's tool is only for posting elsewhere; replies are delivered in code (D-035).
func (b *Bot) Server(instructions string) *mcpserver.Server {
	srv := mcpserver.New(Name, instructions)
	srv.Add(mcpserver.Tool{
		Name: "send_message",
		Description: "Post a message to a Discord channel other than the current conversation. " +
			"Your reply to the current conversation is sent automatically; do not use this for it.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"channel_id": {"type": "string", "description": "Discord channel ID (a numeric snowflake)."},
				"text":       {"type": "string", "description": "Message text. Mention people as @displayname."}
			},
			"required": ["channel_id", "text"]
		}`),
		OpenWorld: true,
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				ChannelID string `json:"channel_id"`
				Text      string `json:"text"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("invalid args: %w", err)
			}
			if strings.TrimSpace(in.ChannelID) == "" || strings.TrimSpace(in.Text) == "" {
				return "", fmt.Errorf("channel_id and text are required")
			}
			// The reply to this conversation is sent in code; a send here would
			// deliver it twice (D-035).
			if sc, ok := scope.From(ctx); ok && sc.Connector == Name && sc.Channel == strings.TrimSpace(in.ChannelID) {
				return "", fmt.Errorf("that is the current conversation; your reply is delivered automatically, so call reply instead")
			}
			id, err := b.Send(ctx, event.Address{Connector: Name, Channel: in.ChannelID}, in.Text)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("sent message %s to channel %s", id, in.ChannelID), nil
		},
	})
	return srv
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

// mentionRe matches user mentions only; `!` is the legacy nickname form.
// Role (`<@&id>`) and channel (`<#id>`) tokens deliberately don't match.
var mentionRe = regexp.MustCompile(`<@!?(\d+)>`)

// rewriteMentions leaves uncached IDs as raw tokens so the ID survives.
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

// rewriteOutboundMentions turns `@displayname` into real pings. Longer names
// go first so a name that prefixes another can't steal its match.
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

// displayName prefers GlobalName (the new display name) over legacy Username.
func displayName(u *discordgo.User) string {
	if u == nil {
		return ""
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}
