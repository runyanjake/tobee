package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/runyanjake/tobee/internal/abilities"
	"github.com/runyanjake/tobee/internal/agent"
	"github.com/runyanjake/tobee/internal/connectors/discord"
	"github.com/runyanjake/tobee/internal/connectors/email"
	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/identity"
	"github.com/runyanjake/tobee/internal/ingest"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/llm/openai"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/sandboxfs"
	"github.com/runyanjake/tobee/internal/scheduler"
	memoryserver "github.com/runyanjake/tobee/internal/servers/memory"
	remindersserver "github.com/runyanjake/tobee/internal/servers/reminders"
	resourcesserver "github.com/runyanjake/tobee/internal/servers/resources"
	statusserver "github.com/runyanjake/tobee/internal/servers/status"
	systemserver "github.com/runyanjake/tobee/internal/servers/system"
	userserver "github.com/runyanjake/tobee/internal/servers/user"
	workspaceserver "github.com/runyanjake/tobee/internal/servers/workspace"
	"github.com/runyanjake/tobee/internal/session"
	"github.com/runyanjake/tobee/internal/taskqueue"
	"github.com/runyanjake/tobee/internal/telemetry"
	"github.com/runyanjake/tobee/internal/workspace"
)

func main() {
	if err := godotenv.Load(); err != nil {
		os.Stderr.WriteString("no .env file found, using environment variables\n")
	}

	setupLogging()
	setupTimezone()

	dataDir := envOr("DATA_DIR", "data")
	promptsDir := envOr("PROMPTS_DIR", "prompts")

	// --- Model (D-039, D-041) ----------------------------------------------
	model := newModel(envOr("AI_PROVIDER", "openai"))

	// --- Storage ------------------------------------------------------------
	memFS, err := sandboxfs.NewFS(dataDir+"/memory", 64*1024)
	if err != nil {
		fatal("memory: init failed", err)
	}
	// Search is real grep, run inside the sandbox (D-061). It has to exist at
	// boot: a missing binary would otherwise surface as a failed tool call
	// mid-turn, and BusyBox grep is close enough to fool a version check only.
	sandboxfs.GrepBin = envOr("GREP_BIN", "grep")
	if err := sandboxfs.CheckGrep(); err != nil {
		fatal("memory: grep unavailable", err)
	}
	areas, err := workspace.LoadAreas(os.Environ(), int64(mustInt("WORKSPACE_MAX_FILE_SIZE", 262144)))
	if err != nil {
		// Orphan _DESC/_READONLY entries are non-fatal; the registry is still usable.
		slog.Warn("workspace: load issues", "err", err)
	}

	// --- Core plumbing ------------------------------------------------------
	abilityReg := abilities.NewRegistry()
	out := delivery.NewRouter()
	queue, err := taskqueue.Open(dataDir+"/tasks", 256)
	if err != nil {
		fatal("tasks: open failed", err)
	}
	abilityReg.Register(queue.Reporter())
	engine := ingest.New(queue)
	abilityReg.Register(engine.Reporter())
	people, err := identity.Load(os.Environ())
	if err != nil {
		fatal("identity: config", err)
	}
	engine.SetIdentities(people)
	memoryserver.LinkIdentities(memFS, people.People())
	memoryserver.MigrateReserved(memFS)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := mcphost.New()
	abilityReg.Register(host.Reporter())
	connect := func(s *mcpserver.Server) {
		if err := host.ConnectInProcess(ctx, s); err != nil {
			fatal("mcphost: built-in server failed", err)
		}
	}
	instructions := func(name string) string { return readServerPrompt(promptsDir, name) }

	// --- Connectors: input source + reply channel + MCP server ------------
	if token := os.Getenv("DISCORD_TOKEN"); token != "" {
		bot, err := discord.New(discord.Config{Token: token, ChannelID: os.Getenv("DISCORD_CHANNEL_ID")})
		if err != nil {
			fatal("discord: init failed", err)
		}
		out.Register(discord.Name, bot)
		mustRegister(engine, bot)
		engine.Allow(discord.Name, splitList(os.Getenv("DISCORD_ALLOWED_USERS")))
		connect(bot.Server(instructions("discord")))
		abilityReg.Register(bot.Reporter())
	}
	if imapAddr := os.Getenv("EMAIL_IMAP_ADDR"); imapAddr != "" {
		mb, err := email.New(email.Config{
			IMAPAddr: imapAddr,
			SMTPAddr: os.Getenv("EMAIL_SMTP_ADDR"),
			Username: os.Getenv("EMAIL_USERNAME"),
			Password: os.Getenv("EMAIL_PASSWORD"),
			From:     os.Getenv("EMAIL_FROM"),
			Allowed:  splitList(os.Getenv("EMAIL_ALLOWED")),
			Mailbox:  os.Getenv("EMAIL_MAILBOX"),
			Interval: mustDuration("EMAIL_POLL_INTERVAL", time.Minute),
		})
		if err != nil {
			fatal("email: init failed", err)
		}
		out.Register(email.Name, mb)
		mustRegister(engine, mb)
		engine.Allow(email.Name, mb.Allowed())
		connect(mb.Server(instructions("email")))
		abilityReg.Register(mb.Reporter())
	}
	if len(out.Names()) == 0 {
		fatal("no connectors configured", fmt.Errorf("set DISCORD_TOKEN or EMAIL_IMAP_ADDR"))
	}

	// --- Scheduled jobs: an input source with its own MCP server ----------
	jobStore, err := scheduler.NewJobStore(dataDir + "/scheduler/jobs")
	if err != nil {
		fatal("jobs: store init failed", err)
	}
	jobs := scheduler.NewJobManager(jobStore)
	mustRegister(engine, jobs)
	abilityReg.Register(jobs.Reporter())

	// --- Built-in MCP servers ---------------------------------------------
	connect(memoryserver.New(instructions("memory"), memFS))
	connect(statusserver.New(instructions("status"), abilityReg))
	connect(remindersserver.New(instructions("reminders"), jobs))
	connect(userserver.New(instructions("user"), out))
	connect(resourcesserver.New(instructions("resources"), host))
	if sys, err := systemserver.New(promptsDir + "/system"); err != nil {
		slog.Error("prompts: MISSING — agent will misbehave (check PROMPTS_DIR mount)", "err", err)
	} else {
		connect(sys)
	}
	if areas.Len() > 0 {
		connect(workspaceserver.New(instructions("workspace"), areas))
	}

	// --- External MCP servers ---------------------------------------------
	servers, err := mcphost.LoadServers(os.Environ())
	if err != nil {
		slog.Warn("mcphost: config issues", "err", err)
	}
	for _, sc := range servers {
		// A third-party server being down is not a reason to stay down.
		if err := host.Connect(ctx, sc.Name, sc.Transport(), sc.Options()); err != nil {
			slog.Error("mcphost: external server unavailable; continuing without it",
				"server", sc.Name, "err", err)
			continue
		}
		if len(sc.Subscribe) > 0 {
			mustRegister(engine, mcphost.NewResourceSource(host, sc.Name, sc.Subscribe, sc.ReplyTo))
		}
	}
	slog.Info("mcphost: catalog ready", "tools", len(host.ToolNames()))

	// --- Prompts ------------------------------------------------------------
	// One system prompt for every turn (D-029); server sections come from instructions (D-033).
	pinned, err := host.Pinned(ctx)
	if err != nil {
		slog.Error("prompts: pinned resources unreadable", "err", err)
	}
	states, err := agent.LoadStateTemplates(promptsDir + "/state")
	if err != nil {
		fatal("prompts: state templates failed", err)
	}
	logPromptsLoaded(promptsDir, pinned, states.Names())

	// --- Sessions ------------------------------------------------------------
	// Opened after the prompts: closing a session can draw lessons from it,
	// which needs the reflect template (D-052).
	var reflector *agent.Reflector
	if envBool("AGENT_REFLECT", true) {
		reflector = agent.NewReflector(model, states, func(person string, lessons []string) error {
			return memoryserver.AppendLessons(memFS, person, lessons, time.Now())
		})
	}
	sessions, err := session.Open(dataDir+"/sessions", mustDuration("SESSION_IDLE_TIMEOUT", 10*time.Minute),
		func(sess *session.Session) error {
			// The transcript is the durable record and its error retries the
			// archive; reflection is best-effort and never blocks it.
			if err := memoryserver.ArchiveTranscript(memFS, sess.Person, sess.Started, sess.Markdown()); err != nil {
				return err
			}
			reflector.Reflect(ctx, sess)
			return nil
		})
	if err != nil {
		fatal("session: open failed", err)
	}

	// --- Agent ----------------------------------------------------------------
	ctxb := &agent.ContextBuilder{Host: host}
	strategy := newStrategy(envOr("AGENT_STRATEGY", "react"), model, host, states, out)
	runtime := agent.NewRuntime(queue, ctxb, out, strategy, agent.Config{
		TurnBudget: mustDuration("AGENT_TURN_BUDGET", 2*time.Minute),
	}).WithSessions(sessions)

	// --- Lifecycle ------------------------------------------------------------
	engine.Start(ctx)
	runtime.Start(ctx)
	go sessions.Run(ctx, 30*time.Second)
	slog.Info("tobee is running — press Ctrl+C to exit",
		"sources", engine.Names(), "channels", out.Names())

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc

	slog.Info("shutting down...")
	cancel()
	engine.Wait()
	host.Close()
}

// newModel builds the AI_PROVIDER backend; wire quirks stay inside it (D-041).
func newModel(name string) llm.Model {
	switch name {
	case "openai":
		temp := mustFloat("AI_TEMPERATURE", openai.DefaultTemperature)
		url, modelName := mustEnv("AI_PROVIDER_URL"), envOr("AI_MODEL", "local-model")
		slog.Info("llm: configured", "provider", name, "url", url, "model", modelName,
			"temperature", temp, "api_key", os.Getenv("AI_API_KEY") != "")
		return openai.New(openai.Options{
			BaseURL:     url,
			Model:       modelName,
			APIKey:      os.Getenv("AI_API_KEY"),
			Temperature: &temp,
			MaxTokens:   mustInt("AI_MAX_TOKENS", 2048),
			Timeout:     mustDuration("AI_TIMEOUT", 10*time.Minute),
		})
	default:
		fatal("AI_PROVIDER: unknown provider", fmt.Errorf("%q (known: openai)", name))
		return nil
	}
}

// newStrategy builds the reasoning strategy named by AGENT_STRATEGY (D-037).
func newStrategy(name string, model llm.Model, host *mcphost.Host, states *agent.StateTemplates, out *delivery.Router) agent.Strategy {
	switch name {
	case "react":
		return agent.NewLoop(model, host, states, out)
	default:
		fatal("AGENT_STRATEGY: unknown strategy", fmt.Errorf("%q (known: react)", name))
		return nil
	}
}

func mustRegister(e *ingest.Engine, src ingest.Source) {
	if err := e.Register(src); err != nil {
		fatal("ingest: register failed", err)
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

// setupLogging applies LOG_FORMAT, LOG_LEVEL, and LOG_CONTENT_LIMIT (D-040).
func setupLogging() {
	raw := strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	level, levelErr := parseLogLevel(raw)
	opts := &slog.HandlerOptions{Level: level}

	var inner slog.Handler
	format := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT")))
	switch format {
	case "json":
		inner = slog.NewJSONHandler(os.Stderr, opts)
	default:
		inner = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(telemetry.NewHandler(inner)))

	if levelErr != nil {
		slog.Warn("LOG_LEVEL: unrecognised value; using info", "value", raw)
	}
	if format != "" && format != "text" && format != "json" {
		slog.Warn("LOG_FORMAT: unrecognised value; using text", "value", format)
	}
	telemetry.SetContentLimit(mustInt("LOG_CONTENT_LIMIT", 4000))
}

// setupTimezone pins time.Local from TZ. The container has no zone of its
// own, so without this every wall-clock time the model reads in <context>
// and writes into a reminder is UTC, and "4:40pm" needs an offset it was
// never given.
func setupTimezone() {
	name := strings.TrimSpace(os.Getenv("TZ"))
	if name == "" {
		slog.Warn("TZ: not set; wall-clock times use " + time.Local.String() + " (UTC in a container)")
		return
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		slog.Error("TZ: unknown zone; keeping "+time.Local.String(), "tz", name, "err", err)
		return
	}
	time.Local = loc
	slog.Info("clock: local zone set", "tz", loc.String(), "now", time.Now().Format(time.RFC3339))
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

// envBool accepts the same spellings as the workspace read-only flag.
func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return fallback
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func mustInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		fatal(key+": invalid integer", err)
	}
	return v
}

func mustFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fatal(key+": invalid float", err)
	}
	return v
}

func mustDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		fatal(key+": invalid duration", err)
	}
	return v
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// readServerPrompt loads prompts/servers/<name>.md; a missing file just means no guidance.
func readServerPrompt(dir, name string) string {
	path := filepath.Join(dir, "servers", name+".md")
	body, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("prompts: server instructions missing", "path", path, "err", err)
		return ""
	}
	return strings.TrimSpace(string(body))
}

// logPromptsLoaded errors loudly on empty prompts: an unmounted prompts dir
// otherwise looks healthy while the LLM runs with no instructions.
func logPromptsLoaded(dir string, pinned []mcphost.Pinned, stateNames []string) {
	chars := 0
	for _, p := range pinned {
		chars += len(p.Text)
	}
	slog.Info("prompts: loaded",
		"prompts_dir", dir,
		"pinned_resources", len(pinned),
		"system_chars", chars,
		"state_templates", strings.Join(stateNames, ", "))
	missing := []string{}
	if chars == 0 {
		missing = append(missing, "system/*.md")
	}
	required := []string{"turn", "reflect"}
	have := make(map[string]bool, len(stateNames))
	for _, n := range stateNames {
		have[n] = true
	}
	for _, r := range required {
		if !have[r] {
			missing = append(missing, "state/"+r+".md")
		}
	}
	if len(missing) > 0 {
		slog.Error("prompts: MISSING — agent will misbehave (check PROMPTS_DIR mount)",
			"prompts_dir", dir, "missing", strings.Join(missing, ", "))
	}
}
