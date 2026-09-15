# tobee — Claude Code Project Context

## Project Overview

tobee is a self-hosted personal AI assistant, written in Go as one long-running process. It gets messages from Discord and from its own scheduled timers. Each message runs through a plan → execute → synthesize loop against a local, OpenAI-compatible LLM, which must support native tool calling. The reply goes back to the channel the message came from. Everything it remembers is plain text under `data/`. There is no database, no vector store, and no chat history carried between turns.

## Tech Stack & Tooling

- **Language:** Go 1.25 (`go.mod`: `go 1.25.0`). Module path: `github.com/runyanjake/tobee`.
- **Direct dependencies:** `github.com/bwmarrin/discordgo` v0.29.0, `github.com/joho/godotenv` v1.5.1, `github.com/robfig/cron/v3` v3.0.1.
- **Package manager:** Go modules. There is no Makefile, task runner, or external linter config.
- **LLM backend:** any server exposing `/v1/chat/completions` with `tools` and `tool_choice`.
  - Dev: LM Studio on the host (`docker-compose.yml`, `host.docker.internal:1234`).
  - Prod: Ollama container with an Nvidia GPU (`docker-compose.prod.yml`). The Jenkinsfile deploys `qwen2.5:7b`.
- **Container images:** build on `golang:1.25-alpine`, run on `alpine:3.20`.
- **CI/CD:** Jenkins (`Jenkinsfile`). Stages: lint, preflight GPU check, `compose up`, health check, boot-log smoke test, and a Discord webhook notification.

## Directory Structure

| Path | Purpose |
|---|---|
| `cmd/tobee/` | `main.go`: env parsing and wiring only. |
| `internal/agent/` | Turn loop, planner / executor / synthesizer, context builder, reply router, state templates. |
| `internal/llm/` | OpenAI-compatible chat client and wire types. |
| `internal/integrations/` | `Integration` interface, `Envelope`, event bus; `discord/` adapter. |
| `internal/scheduler/` | Static tick scheduler (no ticks registered) and the persistent `JobManager` for model-created jobs. |
| `internal/tools/` | Tool registry plus packs: `memory/`, `workspace/`, `schedule/`, `status/`, `datedname/` (filename stamping helper). |
| `internal/abilities/` | `Reporter` contract and registry behind the `status.*` tools. |
| `internal/sandboxfs/` | Path-sandboxed filesystem backing memory and workspace areas. |
| `internal/workspace/` | Parses `WORKSPACE_AREA_*` env vars into sandboxed areas. |
| `internal/scope/` | Per-turn user/channel scope carried on `context.Context`. |
| `prompts/system/` | System prompt fragments, concatenated in filename order. |
| `prompts/state/` | Per-phase directive templates (`plan`, `execute_step`, `synthesize`). |
| `static/images/` | Cat photos. Not referenced by code. |
| `data/` | Runtime state (gitignored): `memory/`, `scheduler/jobs/`. |
| `.claude/` | This knowledge base. |

## Developer Workflow Commands

```bash
# Install dependencies
go mod download

# Configure (dev)
cp .env.example .env            # set DISCORD_TOKEN; point AI_PROVIDER_URL at LM Studio

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

- Don't build deferred features without discussing them first: vector search, reflection passes, MCP, streaming replies, provider abstraction. See [GOALS.md](GOALS.md#non-goals--out-of-scope).
- No backwards-compatibility shims or parallel old/new code paths. Pick one path.
- Don't add an interface until a second implementation exists. There is one `llm.Client` and one `sandboxfs.FS`.
- Don't add parsers that recover tool calls the model wrote as text, and don't accept prose where a phase requires a tool call (D-025). A salvage parser was built and reverted in `3e818f9`.

### Go style

- Idiomatic Go, formatted with `gofmt`. CI fails on `gofmt -l` output or `go vet` errors.
- Comments explain *why*. Keep them short. Doc-comment exported symbols whose names aren't self-explanatory.
- Wrap errors at package boundaries: `fmt.Errorf("<context>: %w", err)`.
- Log with `log/slog` using structured fields. Prefix messages with the subsystem: `"agent: …"`, `"discord: …"`, `"jobs: …"`.
- Tool names are lowercase and dot-namespaced by subsystem (`memory.read`), never by integration.

### Filesystem & memory

- Every read or write under `data/memory/` or a workspace area goes through `sandboxfs.FS`. Never call `os.*` on those paths. `resolve()` is the security boundary (D-003, D-019).
- Never commit `data/`, `.env`, or `.env.prod`.

### Agent loop

- Envelopes are processed one at a time on purpose (D-005). Don't parallelize consumption.
- No state carries across turns (D-027). Don't reintroduce session buffers or summarizers. Persistence goes through `memory.*`.
- Keep the turn budget and the per-step / total step budgets. Don't raise or remove them to make one case work.
- Every model-authored output is a required virtual tool call: `plan.commit`, `step.finish`, `reply.commit`.
- Anything that must be shown to the user word for word is enforced in code (`tools.Spec.Verbatim`), never by prompt instruction (D-030).
- Never merge the user's text into a phase template. Directives go in `<phase>` tags (D-029).

### Integrations & tools

- Every inbound event is an `integrations.Envelope`. Inbound goes through `bus.Publish`; outbound goes through `Replies` (sender / editor / reactor).
- Replies and reactions are not tools. Tools are for actions the model chooses.
- New tools need a real JSON-Schema `InputSchema` and a bullet in `prompts/system/05-tools.md` (D-028).

### Prompts & config

- Prompt text lives in `prompts/`, not in Go string literals. Existing exceptions:
  - protocol nudges and virtual-tool schemas in `planner.go`, `executor.go`, `synthesizer.go`
  - tool `Description` fields
  - the `<memory>` hint in `context.go`
- Prompts are baked into the prod image (a rebuild ships changes). Dev compose bind-mounts them (a restart picks up changes).
- `.env` is the config surface. New variables go in `.env.example` with a one-line comment and are read in `cmd/tobee/main.go`.
- TODO: The runtime image has no `USER` directive, so the container runs as root. Decide whether non-root is required.

### Git

- Commit subjects are imperative, sentence case, with no type prefix and no trailing period (e.g. `Let the planner answer directly; zero steps is the fast path`).
- Commit bodies explain the failure, the fix, and the cost, citing `D-0xx` IDs.
- AI-assisted commits end with a `Co-Authored-By:` trailer.
- Branches: the existing local branches use descriptive kebab-case (`synth-slim-context-violations`). TODO: Confirm the branch naming convention and which branch Jenkins deploys from.

### Documentation

- A design decision change adds a new `D-0xx` row in [DESIGN.md](DESIGN.md#key-decisions). Mark the old row superseded and log the change in [IMPLEMENTATION.md](IMPLEMENTATION.md). Never reuse an ID: code comments cite them. The next free ID is **D-033**.
- Routine code changes don't need doc edits. Update docs when shape, contracts, or config change.

### Working with the user

- Be terse. State decisions; don't narrate deliberation.
- Ask before destructive actions: deleting files, wiping `data/`, force-pushing, rewriting history.

## Knowledge Base Links

- [GOALS.md](GOALS.md): capabilities, audience, environment, non-goals, priorities.
- [DESIGN.md](DESIGN.md): architecture, turn lifecycle, reasoning patterns, memory model, key decisions, rejected alternatives.
- [IMPLEMENTATION.md](IMPLEMENTATION.md): history, milestones, superseded decisions, known limitations, technical debt.
- [GLOSSARY.md](GLOSSARY.md): project terms and protocols.
