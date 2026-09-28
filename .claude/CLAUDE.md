# tobee — Claude Code Project Context

## Project Overview

tobee is a self-hosted personal AI assistant, written in Go as one long-running process. It is an MCP host. Input comes from pluggable ingest sources (Discord, email, its own scheduled jobs, MCP resource notifications) through a durable task queue. A single serial runtime runs each task through a reasoning strategy (today: plan → execute → synthesize) against an OpenAI-compatible LLM with native tool calling. Every tool, built-in or third-party, is served by an MCP server. The reply goes back to where the event came from. Everything it remembers is plain text under `data/`. There is no database, no vector store, and no chat history carried between turns; only a parked clarifying question survives a turn.

## Tech Stack & Tooling

- **Language:** Go 1.25 (`go.mod`: `go 1.25.0`). Module path: `github.com/runyanjake/tobee`.
- **Direct dependencies:**
  - `github.com/modelcontextprotocol/go-sdk` v1.8.0 (the official MCP SDK)
  - `github.com/bwmarrin/discordgo` v0.29.0
  - `github.com/emersion/go-imap/v2` v2.0.0-beta.8 and `github.com/emersion/go-message` v0.18.2 (email)
  - `github.com/joho/godotenv` v1.5.1
  - `github.com/robfig/cron/v3` v3.0.1
- **Package manager:** Go modules. There is no Makefile, task runner, or external linter config.
- **LLM backend:** any server exposing `/v1/chat/completions` with `tools` and `tool_choice`. Selected entirely by `AI_*` env vars, including `AI_API_KEY` for hosted APIs (D-039).
  - Dev: LM Studio on the host (`docker-compose.yml`, `host.docker.internal:1234`).
  - Prod: Ollama container with an Nvidia GPU (`docker-compose.prod.yml`). The Jenkinsfile deploys `qwen2.5:7b`.
- **Container images:** build on `golang:1.25-alpine`, run on `alpine:3.20`.
- **CI/CD:** Jenkins (`Jenkinsfile`). Stages: lint, preflight GPU check, `compose up`, health check, boot-log smoke test (`tobee is running` and `discord: connected`), and a Discord webhook notification.

## Directory Structure

| Path | Purpose |
|---|---|
| `cmd/tobee/` | `main.go`: env parsing and wiring only. |
| `internal/event/` | `Event`, `Address`, `Actor`, resume keys. Shared by every layer. |
| `internal/ingest/` | `Source` interface and `Engine`: supervision, dedup, allowlists. |
| `internal/taskqueue/` | Durable task queue and parked tasks under `data/tasks/`. |
| `internal/agent/` | `Runtime`, `Strategy`, `PlanExecute` (planner / executor / synthesizer), context builder, state templates. |
| `internal/mcphost/` | MCP host: sessions, catalog, trust, `MCP_SERVER_*` config, resource-subscription source. |
| `internal/mcpserver/` | Builder for built-in MCP servers (scope from `_meta`, error results, tobee metadata). |
| `internal/servers/` | Built-in MCP servers: `memory/`, `workspace/`, `schedule/`, `status/`, `user/`. |
| `internal/connectors/` | `discord/` and `email/`: each is a source, a delivery channel, and an MCP server. |
| `internal/delivery/` | `Router` from connector name to `Channel` / `Editor` / `Reactor`. |
| `internal/llm/` | OpenAI-compatible chat client and wire types. |
| `internal/scheduler/` | `JobManager`: model-created jobs, an ingest source. |
| `internal/abilities/` | `Reporter` contract and registry behind the `status_*` tools. |
| `internal/sandboxfs/` | Path-sandboxed filesystem backing memory and workspace areas. |
| `internal/workspace/` | Parses `WORKSPACE_AREA_*` env vars into sandboxed areas. |
| `internal/scope/` | Per-turn user/channel scope on `context.Context` and in MCP `_meta`. |
| `internal/datedname/` | Filename date-stamping helper. |
| `internal/telemetry/` | Log categories and correlation (D-040). |
| `prompts/system/` | System prompt fragments, concatenated in filename order. |
| `prompts/servers/` | MCP `instructions` for each built-in server, one file per server name. |
| `prompts/state/` | Per-phase directive templates (`plan`, `execute_step`, `synthesize`). |
| `static/images/` | Cat photos. Not referenced by code. |
| `data/` | Runtime state (gitignored): `memory/`, `scheduler/jobs/`, `tasks/`. |
| `.claude/` | This knowledge base. |

## Developer Workflow Commands

```bash
# Install dependencies
go mod download

# Configure (dev)
cp .env.example .env            # set DISCORD_TOKEN and/or EMAIL_*; point AI_PROVIDER_URL at LM Studio

# Run natively (godotenv reads ./.env). Outside Docker, use a host URL:
AI_PROVIDER_URL=http://localhost:1234 go run ./cmd/tobee

# Run in Docker (dev: prompts and data bind-mounted, LLM on host)
docker compose up --build

# Build
go build ./cmd/tobee

# Test
go test ./...

# Format / lint (same checks CI runs)
gofmt -w .                      # format
gofmt -l .                      # must print nothing
go vet ./...
docker build --target lint .    # CI lint stage

# Prod (Linux + Nvidia GPU)
cp .env.prod.example .env.prod
docker compose -f docker-compose.prod.yml up -d --build
docker compose -f docker-compose.prod.yml logs -f tobee
```

## Agent Guidelines & Guardrails

### Scope

- Don't build deferred features without discussing them first: vector search, reflection passes, exposing tobee as an MCP server, streaming replies, a non-OpenAI provider abstraction. See [GOALS.md](GOALS.md#non-goals--out-of-scope).
- No backwards-compatibility shims or parallel old/new code paths. Pick one path.
- Don't add an interface until a second implementation exists. There is one `llm.Client` and one `sandboxfs.FS`. `agent.Strategy` is the deliberate exception (D-037).
- Don't add parsers that recover tool calls the model wrote as text, and don't accept prose where a phase requires a tool call (D-025). A salvage parser was built and reverted in `3e818f9`.

### Go style

- Idiomatic Go, formatted with `gofmt`. CI fails on `gofmt -l` output or `go vet` errors.
- Comments explain *why*. Keep them short. Doc-comment exported symbols whose names aren't self-explanatory.
- Wrap errors at package boundaries: `fmt.Errorf("<context>: %w", err)`.
- Log with `log/slog` using structured fields. Prefix messages with the subsystem: `"agent: …"`, `"discord: …"`, `"jobs: …"`.
- Log prefixes for the new layers: `"ingest: …"`, `"taskqueue: …"`, `"mcphost: …"`, `"mcpserver: …"`, `"email: …"`.
- Anything in the reasoning chain logs through `telemetry.Log(ctx, level, telemetry.<Category>, …)` with the turn's `ctx`, so it carries `cat`, `task`, `phase`, and `step`. Wrap message and tool text in `telemetry.Content` so it is capped. Plain `slog` calls are fine elsewhere; they are tagged `cat=system` automatically. Don't log the whole conversation per LLM call; `callLLM` logs only new messages (D-040).
- Tools live on an MCP server and are exposed as `<server>_<tool>` (`memory_read`). Server names are lowercase `[a-z0-9_-]`. Never use dots: hosted APIs reject them (D-033).

### Filesystem & memory

- Every read or write under `data/memory/` or a workspace area goes through `sandboxfs.FS`. Never call `os.*` on those paths. `resolve()` is the security boundary (D-003, D-019).
- Never commit `data/`, `.env`, or `.env.prod`.

### Agent loop

- Tasks are processed one at a time on purpose (D-005). Don't parallelize consumption.
- No chat history carries across turns (D-027). Don't reintroduce session buffers or summarizers. Persistence goes through `memory_*`. The only exception is a parked task, which holds the request and the question, never a transcript (D-036).
- New reasoning schemes implement `agent.Strategy` and are selected by `AGENT_STRATEGY` (D-037). The runtime owns scope, budget, delivery, and parking; a strategy only fills `Turn.Reply` or `Turn.Await`.
- Keep the turn budget and the per-step / total step budgets. Don't raise or remove them to make one case work.
- Every model-authored output is a required virtual tool call: `plan_commit`, `step_finish`, `reply_commit`.
- Anything that must be shown to the user word for word is enforced in code (`mcpserver.Tool.Verbatim` → `tobee/verbatim`), never by prompt instruction (D-030).
- Never merge the user's text into a phase template. Directives go in `<phase>` tags (D-029).

### Connectors, sources & tools

- Every input is an `ingest.Source` emitting `event.Event`s. Register it with the ingest engine in `main.go`; never write to the task queue directly. Give events a stable `ID`: it is the dedup key (D-034).
- A new external system is a connector under `internal/connectors/<name>/`: a `Source`, a `delivery.Channel` (plus `Editor` / `Reactor` if supported), an MCP server, and a Reporter, all named with one `Name` constant (D-035).
- The reply to the event's origin, and progress reactions, are delivered in code, not by tools. Tools are for actions the model chooses, including messages elsewhere and `user_ask` (D-035).
- New capabilities are MCP tools, never a side channel. A built-in server uses `internal/mcpserver`, gets a `prompts/servers/<name>.md` instructions file, and is connected with `host.ConnectInProcess`. Every tool needs a real JSON-Schema `InputSchema`; set `ReadOnly` when it doesn't write (D-033).
- Third-party tools arrive through `MCP_SERVER_<NAME>_*`, not code. Don't relax trust: untrusted servers get no scope, no prompt instructions, no verbatim/await, and no subscriptions (D-038).
- A source that admits third-party text (like email) needs an allowlist, enforced in code.

### Prompts & config

- Prompt text lives in `prompts/`, not in Go string literals. Existing exceptions:
  - protocol nudges and virtual-tool schemas in `planner.go`, `executor.go`, `synthesizer.go`
  - tool `Description` fields
  - the `<servers>` / `<context>` scaffolding in `context.go`
  - the workspace area list appended to its instructions
- Prompts are baked into the prod image (a rebuild ships changes). Dev compose bind-mounts them (a restart picks up changes).
- `.env` is the config surface. New variables go in `.env.example` with a one-line comment and are read in `cmd/tobee/main.go`.
- TODO: The runtime image has no `USER` directive, so the container runs as root. Decide whether non-root is required.

### Git

- Commit subjects are imperative, sentence case, with no type prefix and no trailing period (e.g. `Let the planner answer directly; zero steps is the fast path`).
- Commit bodies explain the failure, the fix, and the cost, citing `D-0xx` IDs.
- AI-assisted commits end with a `Co-Authored-By:` trailer.
- Branches: the existing local branches use descriptive kebab-case (`synth-slim-context-violations`). TODO: Confirm the branch naming convention and which branch Jenkins deploys from.

### Documentation

- A design decision change adds a new `D-0xx` row in [DESIGN.md](DESIGN.md#key-decisions). Mark the old row superseded and log the change in [IMPLEMENTATION.md](IMPLEMENTATION.md). Never reuse an ID: code comments cite them. The next free ID is **D-041**.
- Routine code changes don't need doc edits. Update docs when shape, contracts, or config change.

### Working with the user

- Be terse. State decisions; don't narrate deliberation.
- Ask before destructive actions: deleting files, wiping `data/`, force-pushing, rewriting history.

## Knowledge Base Links

- [GOALS.md](GOALS.md): capabilities, audience, environment, non-goals, priorities.
- [DESIGN.md](DESIGN.md): architecture, turn lifecycle, reasoning patterns, memory model, key decisions, rejected alternatives.
- [IMPLEMENTATION.md](IMPLEMENTATION.md): history, milestones, superseded decisions, known limitations, technical debt.
- [GLOSSARY.md](GLOSSARY.md): project terms and protocols.
