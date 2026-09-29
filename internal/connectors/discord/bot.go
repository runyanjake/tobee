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

	// Lookups behind the addressing rules. Both are cheap in-memory hits
	// after the first message from a guild or channel.
	rolesMu sync.RWMutex
	roles   map[string]map[string]bool // guild ID → role IDs the bot holds

	chanMu sync.Mutex
	chans  map[string]channelInfo // channel ID → thread shape and membership

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

// channelInfo answers "is this a thread, whose, and are we in it" without a
// REST call per message. Invalidated when the bot posts to the channel.
type channelInfo struct {
	isThread bool
	parentID string
	botIn    bool // the bot is a member of this thread
	at       time.Time
}

const channelInfoTTL = 10 * time.Minute

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
		roles:     make(map[string]map[string]bool),
		chans:     make(map[string]channelInfo),
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
	if !b.inScope(s, m) {
		return
	}

	b.remember(m.Author)
	for _, u := range m.Mentions {
		b.remember(u)
	}

	addressed, why := b.isAddressed(s, m)
	if !addressed {
		mentionIDs := make([]string, 0, len(m.Mentions))
		for _, u := range m.Mentions {
			if u != nil {
				mentionIDs = append(mentionIDs, u.ID)
			}
		}
		slog.Debug("discord: ambient (not addressed); dropping",
			"channel", m.ChannelID, "author", displayName(m.Author),
			"self_id", s.State.User.ID, "is_dm", m.GuildID == "",
			"mentions", mentionIDs, "roles", m.MentionRoles, "content", m.Content)
		return
	}

	content := strings.TrimSpace(b.rewriteMentions(b.rewriteRoles(s, m.GuildID, m.Content)))
	if content == "" {
		return
	}

	authorName := displayName(m.Author)
	b.recordRx(m.ChannelID)
	slog.Debug("discord: message received",
		"channel", m.ChannelID, "author", authorName,
		"author_id", m.Author.ID, "is_dm", m.GuildID == "",
		"addressed_by", why, "content", content)

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

// isAddressed decides whether a message is for tobee and says which rule
// admitted it (D-050). Cheap in-memory rules run before any lookup, so an
// ambient message costs nothing but a regex.
func (b *Bot) isAddressed(s *discordgo.Session, m *discordgo.MessageCreate) (bool, string) {
	selfID := s.State.User.ID
	if ok, why := addressedInMessage(m, selfID); ok {
		return true, why
	}
	switch {
	case b.mentionsOwnRole(s, m, selfID):
		return true, "role"
	case b.inOwnThread(s, m.ChannelID, selfID):
		return true, "thread"
	}
	return false, ""
}

// addressedInMessage holds the rules the message answers by itself, with no
// guild or channel lookup.
func addressedInMessage(m *discordgo.MessageCreate, selfID string) (bool, string) {
	switch {
	case m.GuildID == "":
		// A DM has no ambient chatter: every message in it is the request.
		return true, "dm"
	case mentionsUser(m, selfID):
		return true, "mention"
	case repliesToUser(m, selfID):
		return true, "reply"
	case nameRe.MatchString(m.Content):
		return true, "name"
	}
	return false, ""
}

// mentionsUser also checks the raw tokens: a mention can be missing from
// m.Mentions on a gateway state race. `!` is the legacy nickname form.
func mentionsUser(m *discordgo.MessageCreate, userID string) bool {
	if userID == "" {
		return false
	}
	for _, u := range m.Mentions {
		if u != nil && u.ID == userID {
			return true
		}
	}
	return strings.Contains(m.Content, "<@"+userID+">") || strings.Contains(m.Content, "<@!"+userID+">")
}

func repliesToUser(m *discordgo.MessageCreate, userID string) bool {
	ref := m.ReferencedMessage
	return userID != "" && ref != nil && ref.Author != nil && ref.Author.ID == userID
}

// mentionsOwnRole catches "@TOBEE" resolving to the bot's integration-managed
// role rather than its user: Discord sends <@&roleID>, which never appears in
// m.Mentions, so this read as ambient chatter before D-050.
func (b *Bot) mentionsOwnRole(s *discordgo.Session, m *discordgo.MessageCreate, selfID string) bool {
	if len(m.MentionRoles) == 0 || m.GuildID == "" {
		return false
	}
	return pingsRole(m.MentionRoles, m.GuildID, b.selfRoles(s, m.GuildID, selfID))
}

// pingsRole reports whether any pinged role is one of own. The @everyone role's
// ID is the guild's own, and pinging everyone is not addressing tobee.
func pingsRole(pinged []string, guildID string, own map[string]bool) bool {
	for _, id := range pinged {
		if id != "" && id != guildID && own[id] {
			return true
		}
	}
	return false
}

// selfRoles is the set of role IDs the bot holds in a guild. State usually has
// the bot's own member from GUILD_CREATE; the REST fallback is cached because
// roles change far less often than messages arrive.
func (b *Bot) selfRoles(s *discordgo.Session, guildID, selfID string) map[string]bool {
	if selfID == "" {
		return nil
	}
	if member, err := s.State.Member(guildID, selfID); err == nil && member != nil {
		return roleSet(member.Roles)
	}
	b.rolesMu.RLock()
	cached, ok := b.roles[guildID]
	b.rolesMu.RUnlock()
	if ok {
		return cached
	}
	member, err := s.GuildMember(guildID, selfID)
	if err != nil {
		slog.Debug("discord: own guild roles unreadable; role pings will read as ambient",
			"guild", guildID, "err", err)
		return nil
	}
	set := roleSet(member.Roles)
	b.rolesMu.Lock()
	b.roles[guildID] = set
	b.rolesMu.Unlock()
	return set
}

func roleSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// inOwnThread admits every message in a thread tobee is a member of — it is
// already part of that conversation, so follow-ups need no mention.
func (b *Bot) inOwnThread(s *discordgo.Session, channelID, selfID string) bool {
	if selfID == "" {
		return false
	}
	info := b.channelInfo(s, channelID, selfID)
	return info.isThread && info.botIn
}

// inScope applies DISCORD_CHANNEL_ID. A DM is always in scope, and so is a
// thread hanging off the configured channel: a thread has its own channel ID,
// so a plain equality check would drop every message in one.
func (b *Bot) inScope(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if b.channelID == "" || m.GuildID == "" || m.ChannelID == b.channelID {
		return true
	}
	info := b.channelInfo(s, m.ChannelID, s.State.User.ID)
	return info.isThread && info.parentID == b.channelID
}

// channelInfo caches the thread shape of a channel and whether the bot is in
// it. One REST pair per channel per TTL at worst; Send invalidates the entry
// so a thread the bot just posted in is recognised immediately.
func (b *Bot) channelInfo(s *discordgo.Session, channelID, selfID string) channelInfo {
	b.chanMu.Lock()
	if info, ok := b.chans[channelID]; ok && time.Since(info.at) < channelInfoTTL {
		b.chanMu.Unlock()
		return info
	}
	b.chanMu.Unlock()

	info := channelInfo{at: time.Now()}
	ch, err := s.State.Channel(channelID)
	if err != nil || ch == nil {
		if ch, err = s.Channel(channelID); err != nil {
			slog.Debug("discord: channel unreadable; treating as a plain channel",
				"channel", channelID, "err", err)
			b.cacheChannel(channelID, info)
			return info
		}
	}
	if ch.IsThread() {
		info.isThread = true
		info.parentID = ch.ParentID
		if selfID != "" {
			// A 404 here is the normal "not a member" answer.
			_, err := s.ThreadMember(channelID, selfID, false)
			info.botIn = err == nil
		}
	}
	b.cacheChannel(channelID, info)
	return info
}

func (b *Bot) cacheChannel(channelID string, info channelInfo) {
	b.chanMu.Lock()
	b.chans[channelID] = info
	b.chanMu.Unlock()
}

func (b *Bot) forgetChannel(channelID string) {
	b.chanMu.Lock()
	delete(b.chans, channelID)
	b.chanMu.Unlock()
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
	// Posting to a thread joins it; a stale "not a member" would keep the
	// thread rule off for the rest of the TTL.
	b.forgetChannel(to.Channel)
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

// roleRe matches role mentions. "@TOBEE" becomes one of these whenever the
// bot's managed role shares its name, so the raw token would otherwise reach
// the model as noise.
var roleRe = regexp.MustCompile(`<@&(\d+)>`)

// rewriteRoles names the role, leaving unresolvable IDs as raw tokens.
func (b *Bot) rewriteRoles(s *discordgo.Session, guildID, content string) string {
	if guildID == "" || !strings.Contains(content, "<@&") {
		return content
	}
	return roleRe.ReplaceAllStringFunc(content, func(tok string) string {
		m := roleRe.FindStringSubmatch(tok)
		if len(m) != 2 {
			return tok
		}
		role, err := s.State.Role(guildID, m[1])
		if err != nil || role == nil || role.Name == "" {
			return tok
		}
		return "@" + role.Name
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
