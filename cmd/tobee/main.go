package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/runyanjake/tobee/internal/abilities"
	"github.com/runyanjake/tobee/internal/agent"
	"github.com/runyanjake/tobee/internal/integrations"
	"github.com/runyanjake/tobee/internal/integrations/discord"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/sandboxfs"
	"github.com/runyanjake/tobee/internal/scheduler"
	"github.com/runyanjake/tobee/internal/tools"
	memtools "github.com/runyanjake/tobee/internal/tools/memory"
	scheduletools "github.com/runyanjake/tobee/internal/tools/schedule"
	statustools "github.com/runyanjake/tobee/internal/tools/status"
	workspacetools "github.com/runyanjake/tobee/internal/tools/workspace"
	"github.com/runyanjake/tobee/internal/workspace"
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
	sessionIdleTimeoutDM, err := time.ParseDuration(envOr("SESSION_IDLE_TIMEOUT_DM", "168h"))
	if err != nil {
		slog.Error("SESSION_IDLE_TIMEOUT_DM: invalid duration", "err", err)
		os.Exit(1)
	}

	planMaxStepsTotal, err := strconv.Atoi(envOr("PLAN_MAX_STEPS_TOTAL", "12"))
	if err != nil {
		slog.Error("PLAN_MAX_STEPS_TOTAL: invalid integer", "err", err)
		os.Exit(1)
	}
	planMaxStepsPerStep, err := strconv.Atoi(envOr("PLAN_MAX_STEPS_PER_STEP", "4"))
	if err != nil {
		slog.Error("PLAN_MAX_STEPS_PER_STEP: invalid integer", "err", err)
		os.Exit(1)
	}
	planMaxReplans, err := strconv.Atoi(envOr("PLAN_MAX_REPLANS", "3"))
	if err != nil {
		slog.Error("PLAN_MAX_REPLANS: invalid integer", "err", err)
		os.Exit(1)
	}

	// --- LLM client -------------------------------------------------------
	client := llm.NewClient(aiURL, aiModel, llm.Options{
		Temperature: 0.7,
		MaxTokens:   2048,
		Timeout:     10 * time.Minute,
	})

	// --- Memory filesystem ------------------------------------------------
	memFS, err := sandboxfs.NewFS(dataDir+"/memory", 64*1024)
	if err != nil {
		slog.Error("memory: init failed", "err", err)
		os.Exit(1)
	}

	// --- Workspace areas (optional) --------------------------------------
	wsMax, err := parseInt64(envOr("WORKSPACE_MAX_FILE_SIZE", "262144"))
	if err != nil {
		slog.Error("WORKSPACE_MAX_FILE_SIZE: invalid integer", "err", err)
		os.Exit(1)
	}
	areas, err := workspace.LoadAreas(os.Environ(), wsMax)
	if err != nil {
		// Orphan _DESC/_READONLY entries log a warning but don't abort —
		// LoadAreas still returns a usable Areas registry.
		slog.Warn("workspace: load issues", "err", err)
	}

	// --- Abilities registry (cross-subsystem introspection) --------------
	abilityReg := abilities.NewRegistry()

	// --- Tool registry ----------------------------------------------------
	registry := tools.NewRegistry()
	memtools.Register(registry, memFS)
	statustools.Register(registry, abilityReg)
	if areas.Len() > 0 {
		workspacetools.Register(registry, areas)
		slog.Info("workspace: areas registered", "count", areas.Len())
	}
	// schedule.* tools are registered after the JobManager is built below.

	// --- Sessions + summarizer -------------------------------------------
	sessions, err := agent.NewSessionStore(dataDir+"/sessions", 10, sessionIdleTimeout, sessionIdleTimeoutDM)
	if err != nil {
		slog.Error("sessions: init failed", "err", err)
		os.Exit(1)
	}
	persona := readPersona(promptsDir + "/persona")
	plannerPrompt := readFile(promptsDir+"/planner.md", "")
	synthPrompt := readFile(promptsDir+"/synthesizer.md", "")
	summPrompt := readFile(promptsDir+"/summarizer.md", "")
	summarizer := agent.NewSummarizer(client, summPrompt, sessions)

	// --- Context builder + reply table ------------------------------------
	ctxb := &agent.ContextBuilder{
		Persona:   persona,
		Memory:    memFS,
		Workspace: areas,
		Sessions:  sessions,
	}
	replies := agent.NewReplies()

	// --- Planner / executor / synthesizer --------------------------------
	planner := agent.NewPlanner(client, ctxb, registry, plannerPrompt)
	executor := agent.NewExecutor(client, registry, ctxb, planMaxStepsPerStep, planMaxStepsTotal)
	synthesizer := agent.NewSynthesizer(client, ctxb, synthPrompt)

	// --- Event bus + agent loop ------------------------------------------
	bus := integrations.NewBus(64)
	loop := agent.New(bus, sessions, ctxb, replies, summarizer,
		planner, executor, synthesizer,
		agent.Config{
			TurnBudget: 2 * time.Minute,
			MaxReplans: planMaxReplans,
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

	// --- JobManager: dynamic, model-scheduled jobs -----------------------
	jobStore, err := scheduler.NewJobStore(dataDir + "/scheduler/jobs")
	if err != nil {
		slog.Error("jobs: store init failed", "err", err)
		os.Exit(1)
	}
	jobs := scheduler.NewJobManager(bus, jobStore)
	scheduletools.Register(registry, jobs)
	abilityReg.Register(jobs.Reporter())

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
	if err := jobs.Start(ctx); err != nil {
		slog.Error("jobs: start failed", "err", err)
		os.Exit(1)
	}
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
	raw := strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	level, err := parseLogLevel(raw)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if err != nil {
		slog.Warn("LOG_LEVEL: unrecognised value; using info", "value", raw)
	}
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unrecognised log level %q", s)
	}
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

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

func readFile(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("prompt: read failed; falling back", "path", path, "err", err)
		return fallback
	}
	return string(data)
}

// readPersona loads every *.md file in dir, sorted lexicographically, and
// joins their contents with blank lines. The numeric prefix on each filename
// (00-, 01-, …) is the load-order contract — see DECISIONS.md D-012/D-018.
func readPersona(dir string) string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(matches) == 0 {
		slog.Warn("prompt: persona load failed; using empty persona", "dir", dir, "err", err)
		return ""
	}
	sort.Strings(matches)
	var parts []string
	for _, p := range matches {
		body, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("prompt: persona fragment unreadable; skipping", "path", p, "err", err)
			continue
		}
		parts = append(parts, strings.TrimSpace(string(body)))
	}
	return strings.Join(parts, "\n\n")
}
