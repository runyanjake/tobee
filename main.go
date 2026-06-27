package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"tobee/internal/abilities"
	"tobee/internal/agent"
	"tobee/internal/integrations"
	"tobee/internal/integrations/discord"
	"tobee/internal/llm"
	"tobee/internal/memory"
	"tobee/internal/scheduler"
	"tobee/internal/tools"
	memtools "tobee/internal/tools/memory"
	statustools "tobee/internal/tools/status"
)

func main() {
	if err := godotenv.Load(); err != nil {
		os.Stderr.WriteString("no .env file found, using environment variables\n")
	}

	setupLogging()

	aiURL := mustEnv("AI_PROVIDER_URL")
	discordToken := mustEnv("DISCORD_TOKEN")
	aiModel := envOr("AI_MODEL", "local-model")
	discordChannelID := os.Getenv("DISCORD_CHANNEL_ID")
	dataDir := envOr("DATA_DIR", "data")
	promptsDir := envOr("PROMPTS_DIR", "prompts")

	sessionTTL, err := time.ParseDuration(envOr("SESSION_TTL", "168h"))
	if err != nil {
		slog.Error("SESSION_TTL: invalid duration", "err", err)
		os.Exit(1)
	}
	sessionIdleTimeout, err := time.ParseDuration(envOr("SESSION_IDLE_TIMEOUT", "4h"))
	if err != nil {
		slog.Error("SESSION_IDLE_TIMEOUT: invalid duration", "err", err)
		os.Exit(1)
	}

	// --- LLM client -------------------------------------------------------
	client := llm.NewClient(aiURL, aiModel, llm.Options{
		Temperature: 0.7,
		MaxTokens:   2048,
		Timeout:     10 * time.Minute,
	})

	// --- Memory filesystem ------------------------------------------------
	memFS, err := memory.NewFS(dataDir + "/memory")
	if err != nil {
		slog.Error("memory: init failed", "err", err)
		os.Exit(1)
	}

	// --- Abilities registry (cross-subsystem introspection) --------------
	abilityReg := abilities.NewRegistry()

	// --- Tool registry ----------------------------------------------------
	registry := tools.NewRegistry()
	memtools.Register(registry, memFS)
	statustools.Register(registry, abilityReg)

	// --- Sessions + summarizer -------------------------------------------
	sessions, err := agent.NewSessionStore(dataDir+"/sessions", 10, sessionIdleTimeout)
	if err != nil {
		slog.Error("sessions: init failed", "err", err)
		os.Exit(1)
	}
	persona := readPersonality(promptsDir + "/personality")
	summPrompt := readFile(promptsDir+"/summarizer.md", "")
	summarizer := agent.NewSummarizer(client, summPrompt, sessions)

	// --- Context builder + reply table ------------------------------------
	ctxb := &agent.ContextBuilder{
		Persona:  persona,
		Memory:   memFS,
		Sessions: sessions,
	}
	replies := agent.NewReplies()

	// --- Event bus + agent loop ------------------------------------------
	bus := integrations.NewBus(64)
	loop := agent.New(bus, client, registry, sessions, ctxb, replies, summarizer, agent.Config{
		MaxSteps:   8,
		TurnBudget: 2 * time.Minute,
	})

	// --- Integrations -----------------------------------------------------
	dbot, err := discord.New(discord.Config{
		Token:     discordToken,
		ChannelID: discordChannelID,
	}, bus, replies)
	if err != nil {
		slog.Error("discord: init failed", "err", err)
		os.Exit(1)
	}
	active := []integrations.Integration{dbot}
	abilityReg.Register(dbot.Reporter())

	// --- Scheduler (no ticks registered day one) --------------------------
	sched := scheduler.New(bus)
	abilityReg.Register(sched.Reporter())

	// --- Janitor: prune stale session summaries --------------------------
	// LLM-free periodic sweep. Only touches data/sessions — long-term memory
	// under data/memory is never deleted.
	janitor := scheduler.NewJanitor(sessions, dataDir+"/sessions",
		sessionIdleTimeout, sessionTTL, time.Hour)
	abilityReg.Register(janitor.Reporter())

	// --- Lifecycle --------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, ig := range active {
		if err := ig.Start(ctx); err != nil {
			slog.Error("integration: start failed", "name", ig.Name(), "err", err)
			os.Exit(1)
		}
	}
	loop.Start(ctx)
	sched.Start(ctx)
	janitor.Start(ctx)

	slog.Info("tobee is running — press Ctrl+C to exit")

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc

	slog.Info("shutting down...")
	cancel()
	for _, ig := range active {
		if err := ig.Stop(); err != nil {
			slog.Error("integration: stop failed", "name", ig.Name(), "err", err)
		}
	}
}

func setupLogging() {
	level := slog.LevelInfo
	if v := os.Getenv("DEBUG"); v == "1" || v == "true" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required environment variable is not set", "key", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func readFile(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("prompt: read failed; falling back", "path", path, "err", err)
		return fallback
	}
	return string(data)
}

// readPersonality loads every *.md file in dir, sorted lexicographically,
// and joins their contents with blank lines. The numeric prefix on each
// filename (00-, 01-, …) is the load-order contract — see DECISIONS.md D-012.
func readPersonality(dir string) string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(matches) == 0 {
		slog.Warn("prompt: personality load failed; using empty persona", "dir", dir, "err", err)
		return ""
	}
	sort.Strings(matches)
	var parts []string
	for _, p := range matches {
		body, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("prompt: personality fragment unreadable; skipping", "path", p, "err", err)
			continue
		}
		parts = append(parts, strings.TrimSpace(string(body)))
	}
	return strings.Join(parts, "\n\n")
}
