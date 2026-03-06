package discord

import (
	"context"
	"fmt"
	"log"
	"strings"
	"tobee/internal/agent"

	"github.com/bwmarrin/discordgo"
)

// Bot is a Discord integration. It implements integration.Integration.
type Bot struct {
	session   *discordgo.Session
	state     *agent.State
	queue     *agent.Queue
	channelID string // if set, only messages in this channel are handled
}

// New creates a Bot but does not connect yet. Call Start to open the connection.
// channelID is optional: pass an empty string to handle messages from all channels.
func New(token string, s *agent.State, q *agent.Queue, channelID string) (*Bot, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("creating discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsAllWithoutPrivileged

	bot := &Bot{
		session:   session,
		state:     s,
		queue:     q,
		channelID: channelID,
	}
	session.AddHandler(bot.onReady)
	session.AddHandler(bot.onGuildCreate)
	session.AddHandler(bot.onMessageCreate)

	// Register the reply handler so the core loop can deliver responses back here.
	s.RegisterAction("reply:discord", bot.replyAction)

	return bot, nil
}

func (b *Bot) Name() string { return "discord" }

// Start opens the WebSocket connection to Discord.
func (b *Bot) Start(_ context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("opening discord connection: %w", err)
	}
	log.Println("discord: bot connected")
	return nil
}

// Stop closes the WebSocket connection.
func (b *Bot) Stop() error {
	return b.session.Close()
}

// onReady logs the bot's identity once the Gateway READY event is received.
func (b *Bot) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	log.Printf("discord: ready — logged in as %s (id=%s)", r.User.Username, r.User.ID)
	log.Printf("discord: member of %d guild(s)", len(r.Guilds))
	if b.channelID != "" {
		log.Printf("discord: listening on channel id=%s", b.channelID)
	} else {
		log.Println("discord: listening on all channels (set DISCORD_CHANNEL_ID to restrict)")
	}
}

// onGuildCreate fires for each guild on connect and logs all text channels.
func (b *Bot) onGuildCreate(_ *discordgo.Session, g *discordgo.GuildCreate) {
	log.Printf("discord: guild %q (id=%s) — text channels:", g.Name, g.ID)
	for _, ch := range g.Channels {
		if ch.Type == discordgo.ChannelTypeGuildText {
			log.Printf("discord:   #%-20s id=%s", ch.Name, ch.ID)
		}
	}
}

// onMessageCreate handles incoming Discord messages.
func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if b.channelID != "" && m.ChannelID != b.channelID {
		return
	}

	content := strings.TrimSpace(m.Content)
	if content == "" {
		return
	}

	log.Printf("discord: recv channel=%s author=%s content=%q", m.ChannelID, m.Author.Username, content)

	// Direct !commands are handled immediately without going through the AI loop.
	if strings.HasPrefix(content, "!") {
		response, err := b.handleCommand(context.Background(), content[1:])
		if err != nil {
			log.Printf("discord: error handling command: %v", err)
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Error: %v", err))
			return
		}
		if response != "" {
			log.Printf("discord: send channel=%s content=%q", m.ChannelID, response)
			if _, sendErr := s.ChannelMessageSend(m.ChannelID, response); sendErr != nil {
				log.Printf("discord: error sending message: %v", sendErr)
			}
		}
		return
	}

	// All other messages are queued for the core loop to process.
	b.queue.Push(agent.Message{
		Integration: "discord",
		SessionID:   m.ChannelID,
		Content:     content,
	})
}

// replyAction is registered as "reply:discord" and called by the core loop to
// deliver a response back to the originating Discord channel.
func (b *Bot) replyAction(ctx context.Context, _ *agent.State, args map[string]string) (string, error) {
	msg, ok := agent.MessageFrom(ctx)
	if !ok {
		return "", fmt.Errorf("reply:discord called without message context")
	}
	for _, chunk := range splitMessage(args["message"]) {
		log.Printf("discord: send channel=%s content=%q", msg.SessionID, chunk)
		if _, err := b.session.ChannelMessageSend(msg.SessionID, chunk); err != nil {
			return "", fmt.Errorf("sending message: %w", err)
		}
	}
	return "", nil
}

// handleCommand parses "actionName key=val bare words" and invokes the matching action directly.
func (b *Bot) handleCommand(ctx context.Context, input string) (string, error) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", nil
	}

	actionName := parts[0]
	args := parseArgs(parts[1:])

	fn, ok := b.state.GetAction(actionName)
	if !ok {
		return fmt.Sprintf("Unknown action: %q. Try !help", actionName), nil
	}

	return fn(ctx, b.state, args)
}

// parseArgs converts ["key=val", "bare", "word"] into {"key": "val", "message": "bare word"}.
func parseArgs(parts []string) map[string]string {
	args := make(map[string]string)
	var bare []string
	for _, p := range parts {
		if idx := strings.Index(p, "="); idx > 0 {
			args[p[:idx]] = p[idx+1:]
		} else {
			bare = append(bare, p)
		}
	}
	if len(bare) > 0 {
		args["message"] = strings.Join(bare, " ")
	}
	return args
}
