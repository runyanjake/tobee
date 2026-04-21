// Package discord is the Discord integration. It connects to the Discord
// gateway, pushes inbound messages onto the agent bus, and registers a
// reply sender so the agent can deliver responses back to the originating
// channel.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"tobee/internal/agent"
	"tobee/internal/integrations"
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
}

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
	content := strings.TrimSpace(m.Content)
	if content == "" {
		return
	}

	slog.Debug("discord: recv",
		"channel", m.ChannelID, "author", m.Author.Username, "content", content)

	b.bus.Publish(integrations.Envelope{
		Integration: "discord",
		User:        m.Author.ID,
		Channel:     m.ChannelID,
		Content:     content,
		Received:    time.Now(),
	})
}

// sendReply is registered on the agent's Replies table. channel is the
// Discord channel ID; thread is unused for now (future: forum/thread support).
func (b *Bot) sendReply(_ context.Context, channel, _, text string) error {
	for _, chunk := range splitMessage(text) {
		slog.Debug("discord: send", "channel", channel, "chars", len(chunk))
		if _, err := b.session.ChannelMessageSend(channel, chunk); err != nil {
			return fmt.Errorf("send: %w", err)
		}
	}
	return nil
}
