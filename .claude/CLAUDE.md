# tobee — Claude Code Project Context

## Project Overview

tobee is a self-hosted personal AI assistant, written in Go as one long-running process. It is an MCP host. Input comes from pluggable ingest sources (Discord, email, its own scheduled jobs, MCP resource notifications) through a durable task queue. A single serial runtime runs each task through a reasoning strategy (today: a tool-calling agent loop that ends when the model calls `reply`) against an LLM behind the `llm.Model` interface, where every call is a schema-constrained choice of one tool. Every tool, built-in or third-party, is served by an MCP server. The reply goes back to where the event came from. Everything it remembers is plain text under `data/`. There is no database and no vector store. Each person, linked across connectors, has one live session of recent exchanges, which is archived to memory as a transcript after inactivity.

## Tech Stack & Tooling

- **Language:** Go 1.25 (`go.mod`: `go 1.25.0`). Module path: `github.com/runyanjake/tobee`.
- **Direct dependencies:**
  - `github.com/modelcontextprotocol/go-sdk` v1.8.0 (the official MCP SDK)
  - `github.com/bwmarrin/discordgo` v0.29.0
  - `github.com/emersion/go-imap/v2` v2.0.0-beta.8 and `github.com/emersion/go-message` v0.18.2 (email)
  - `github.com/joho/godotenv` v1.5.1
  - `github.com/robfig/cron/v3` v3.0.1
- **Package manager:** Go modules. There is no Makefile, task runner, or external linter config.
- **LLM backend:** any server exposing `/v1/chat/completions` with `response_format: json_schema` (structured output; grammar-constrained on Ollama). Selected entirely by `AI_*` env vars, including `AI_PROVIDER` and `AI_API_KEY` (D-039, D-041).
  - Dev: LM Studio on the host (`docker-compose.yml`, `host.docker.internal:1234`).
  - Prod: Ollama container with an Nvidia GPU (`docker-compose.prod.yml`). The Jenkinsfile deploys `qwen2.5:7b`.
- **Container images:** build on `golang:1.25-alpine`, run on `alpine:3.20`.
- **CI/CD:** Jenkins (`Jenkinsfile`). Stages: lint, test, preflight GPU check, `compose up`, health check, boot-log smoke test (`tobee is running` and `discord: connected`), and a Discord webhook notification. Lint and test are Dockerfile targets, so they run against the image rather than the agent's Go toolchain.

## Directory Structure

| Path | Purpose |
|---|---|
| `cmd/tobee/` | `main.go`: env parsing and wiring only. |
| `internal/event/` | `Event`, `Address`, `Actor`, resume keys. Shared by every layer. |
| `internal/ingest/` | `Source` interface and `Engine`: supervision, dedup, allowlists. |
| `internal/taskqueue/` | Durable task queue and parked tasks under `data/tasks/`. |
| `internal/agent/` | `Runtime`, `Strategy`, `Loop` (the ReAct agent loop with `reply` and `plan` tools), context builder, state templates. |
| `internal/mcphost/` | MCP host: sessions, catalog, trust, `MCP_SERVER_*` config, resource-subscription source. |
| `internal/mcpserver/` | Builder for built-in MCP servers (scope from `_meta`, error results, tobee metadata). |
| `internal/servers/` | Built-in MCP servers: `memory/`, `workspace/`, `schedule/`, `status/`, `user/`, `resources/` (the read path), `system/` (pinned prompts). |
| `internal/connectors/` | `discord/` and `email/`: each is a source, a delivery channel, and an MCP server. |
| `internal/delivery/` | `Router` from connector name to `Channel` / `Editor` / `Reactor`. |
| `internal/llm/` | `Model` interface (`Decide`: choose one tool), message types. `openai/`: the OpenAI-compatible provider (structured output, schema sanitizer, tool menu). |
| `internal/scheduler/` | `JobManager`: model-created jobs, an ingest source. |
| `internal/abilities/` | `Reporter` contract and registry behind the `status_*` tools. |
| `internal/sandboxfs/` | Path-sandboxed filesystem backing memory and workspace areas. |
| `internal/workspace/` | Parses `WORKSPACE_AREA_*` env vars into sandboxed areas. |
| `internal/scope/` | Per-turn user/channel scope on `context.Context` and in MCP `_meta`. |
| `internal/telemetry/` | Log categories and correlation (D-040). |
| `internal/identity/` | Links connector accounts to one person (D-045). |
| `internal/session/` | Per-person conversation sessions: history, idle expiry, transcripts (D-046). |
| `prompts/system/` | System prompt fragments, served as pinned resources by the `system` server in filename order (D-042). |
| `prompts/servers/` | MCP `instructions` for each built-in server, one file per server name. |
| `prompts/state/` | `turn.md`: the one directive appended after the user's message. |
| `static/images/` | Cat photos. Not referenced by code. |
| `data/` | Runtime state (gitignored): `memory/`, `scheduler/jobs/`, `tasks/`, `sessions/`. |
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

# Search runs a real grep (D-061), so its behaviour depends on which grep is
# installed. Replay the battery against a specific binary — this is how to
# check a deployment target, and a binary named here must pass, not skip:
TOBEE_GREP_BINS=/usr/bin/grep go test ./internal/sandboxfs/ -run Conformance -v

# Format / lint / test (same checks CI runs)
gofmt -w .                      # format
gofmt -l .                      # must print nothing
go vet ./...
docker build --target lint .    # CI lint stage
docker build --target test .    # CI test stage: go test ./... against the image's GNU grep

# Prod (Linux + Nvidia GPU)
cp .env.prod.example .env.prod
docker compose -f docker-compose.prod.yml up -d --build
docker compose -f docker-compose.prod.yml logs -f tobee
```

## Agent Guidelines & Guardrails

### Scope

- Don't build deferred features without discussing them first: vector search, exposing tobee as an MCP server, streaming replies, a non-OpenAI provider abstraction. See [GOALS.md](GOALS.md#non-goals--out-of-scope).
- Anything pinned into the system prompt needs a hard cap in code, and a turn without a user pins nothing from memory (D-052, D-054). Give it a `PinPriority` below the system fragments' 1 if it changes per turn, so the stable prefix stays cacheable (D-017).
- Before the agent can be expected to check whether it has done something already, the answer has to be in front of it: what exists goes in `<memory-files>`, what was done goes in the exchange's `Outcome`. Where a duplicate would be silent, refuse it in code (D-054).
- A connector's user ID is what pings a person (`delivery.Mentioner`); the display name never is. Only replies the person isn't waiting for are addressed to them.
- No backwards-compatibility shims or parallel old/new code paths. Pick one path.
- Don't add an interface until a second implementation exists. There is one `sandboxfs.FS`. `agent.Strategy` (D-037) and `llm.Model` (D-041) are deliberate exceptions.
- Don't add parsers that recover tool calls the model wrote as text, and don't accept prose where a phase requires a tool call (D-025). A salvage parser was built and reverted in `3e818f9`.

### Go style

- Idiomatic Go, formatted with `gofmt`. CI fails on `gofmt -l` output, `go vet` errors, or a failing test.
- Comments explain *why*. Keep them short. Doc-comment exported symbols whose names aren't self-explanatory.
- Wrap errors at package boundaries: `fmt.Errorf("<context>: %w", err)`.
- Log with `log/slog` using structured fields. Prefix messages with the subsystem: `"agent: …"`, `"discord: …"`, `"jobs: …"`.
- Log prefixes for the new layers: `"ingest: …"`, `"taskqueue: …"`, `"mcphost: …"`, `"mcpserver: …"`, `"email: …"`.
- Every line opens with a `[TAG]` the handler derives from the message, so write messages as `"<subsystem>: <short event>"` and the tag follows (D-058). Keep the event to three words or fewer, or the tag gets cut. Don't pass a tag.
- A way for the loop to stall is a distinct log event with the counters behind it, not a field on a shared line: `turn stalled`, `turn too large`, `turn out of time`, `tool closed` (D-059).
- Anything in the reasoning chain logs through `telemetry.Log(ctx, level, telemetry.<Category>, …)` with the turn's `ctx`, so it carries `cat`, `task`, `phase`, and `step`. Wrap message and tool text in `telemetry.Content` so it is capped. Plain `slog` calls are fine elsewhere; they are tagged `cat=system` automatically. Don't log the whole conversation per LLM call; `decide` logs only new messages (D-040).
- Tools live on an MCP server and are exposed as `<server>_<tool>` (`memory_write`). Server names are lowercase `[a-z0-9_-]`. Never use dots: hosted APIs reject them (D-033).

### Filesystem & memory

- Search is real `grep`, run inside the sandbox through `sandboxfs.Grep` (D-061). Add a capability by adding a typed field that maps to a flag; never parse grep's output into a richer model, and never rebuild in Go what a flag already does. The argv is the security boundary: options are typed, the pattern and `.` go after `--`, `cmd.Dir` is the resolved scope root, and `-r` stays `-r`.
- Looking something up is two rungs, not one: `memory_grep count=true` (or `memory_list name=`) to find the documents, then `context=N` or a `#L..` range to read the part that matters. Spans are blank-line blocks, so nothing assumes a file format (D-056). Search matches names as well as contents, and the conversation archive is out of it unless `history=true` (D-060).
- Every read or write under `data/memory/` or a workspace area goes through `sandboxfs.FS`. Never call `os.*` on those paths. `resolve()` is the security boundary (D-003, D-019).
- tobee's own state is not the model's to edit. Anything code owns goes under `.tobee/` in a scope, or outside `data/memory/` entirely; tool handlers refuse writes and deletes there, and code writes it through the FS directly (D-055). Don't put code-owned state in user space and rely on the model leaving it alone.
- Warn the user about a problem only when it cost them something: record everything on the turn, mark what the loop recovered from, and let a stalled or timed-out turn promote it (D-055, D-057).
- Never commit `data/`, `.env`, or `.env.prod`.

### Agent loop

- Tasks are processed one at a time on purpose (D-005). Don't parallelize consumption.
- History is the person's session, recorded by code: user messages, tool calls with real results, and delivered replies (D-046). Don't store model drafts or summaries in it, and don't key it by channel or connector; key by person (D-045).
- Anything the reply says happened must be backed by code: action lines come from real tool results, and destructive calls go through the code-written approval (D-047). Never make a prompt instruction the only guard against a false claim or a destructive action.
- What went wrong is reported by code too. Record it on the turn with `AddProblem` (or `AddRecovered` when a retry fixed it) and let `renderReply` and the session outcome carry it; a nudge asking the model to admit a failure is not a guard (D-051).
- Text code puts in front of a person speaks as tobee, in the first person, about what it did — Reporter summaries, the failure block, action lines. Counts of connected things are operator detail and belong in `status_report`, not in a reply (D-053). Write the sentence where the fact is known, so `Problem.Detail` is already the finished line.
- When the loop stops making progress, take the option away instead of asking it to stop: an identical repeat closes that tool for the turn by leaving it out of the schema (D-051).
- New reasoning schemes implement `agent.Strategy` and are selected by `AGENT_STRATEGY` (D-037). The runtime owns scope, budget, delivery, and parking; a strategy only fills `Turn.Reply` or `Turn.Await`.
- Keep the turn budget. There is no cap on tool calls: a turn ends when it replies, when three calls in a row return nothing new, or when the clock runs down to the reply reserve (D-057). Bound the resource that actually breaks — time, context size, progress — never the call count.
- Every model call goes through `llm.Model.Decide`: the model picks exactly one of the offered tools. The loop offers the MCP catalog plus its own `reply` and `plan`. Never call a provider directly, and keep request shape, output mode, and wire quirks inside `internal/llm/<provider>` (D-041).
- Keep the turn a single loop (D-043). Don't reintroduce fixed phases (a planning call, per-step executors, a synthesis pass); structure the model needs is a tool it can choose, like `plan`.
- Give every built-in tool honest annotations: `ReadOnly` for reads, `OpenWorld` for anything reaching people or outside systems, `Destructive` for anything that deletes or irreversibly overwrites. Categories are derived from them and only group the menu (D-044); `Destructive` makes the user approve each call (D-047).
- Output that isn't a valid choice is never appended to the conversation; the phase appends its nudge and retries once.
- Anything that must be shown to the user word for word is enforced in code (`mcpserver.Tool.Verbatim` → `tobee/verbatim`), never by prompt instruction (D-030). A verbatim tool renders the finished answer, so the first one to run closes them all for that turn (D-060).
- A note the loop writes to the model is harness text: append it with `AppendHarness` in its own message, never onto a tool result. Anything on a tool result is saved to the session and ends up in a transcript, where a later search will find it (D-060).
- Never merge the user's text into a phase template. Directives go in `<phase>` tags (D-029).

### Connectors, sources & tools

- A deferred action — a fired reminder, a resource notification — runs the full agent loop and retrieves its own context. Don't assemble its message in code or push the payload through the record; give the turn what it needs to find the background (D-059).
- Every input is an `ingest.Source` emitting `event.Event`s. Register it with the ingest engine in `main.go`; never write to the task queue directly. Give events a stable `ID`: it is the dedup key (D-034).
- A new external system is a connector under `internal/connectors/<name>/`: a `Source`, a `delivery.Channel` (plus `Editor` / `Reactor` if supported), an MCP server, and a Reporter, all named with one `Name` constant (D-035).
- The reply to the event's origin, and progress reactions, are delivered in code, not by tools. Tools are for actions the model chooses, including messages elsewhere and `user_ask` (D-035).
- Readable content is an MCP resource or resource template with a URI that mirrors its folder path, read through `resources_read`. A `#L<from>-<to>` fragment is parsed in the host and never sent to a server, so range reads work for third-party servers too (D-056). Don't add per-server read tools. Only trusted servers may pin a resource into the system prompt (D-042). From memory, only the capped `lessons.md` is pinned; facts, preferences and transcripts stay tool-read (D-052).
- Anything that turns a model-supplied path into a file path confines it to its scope root first. `sandboxfs` only confines to its own root.
- New capabilities are MCP tools, never a side channel. A built-in server uses `internal/mcpserver`, gets a `prompts/servers/<name>.md` instructions file, and is connected with `host.ConnectInProcess`. Every tool needs a real JSON-Schema `InputSchema`; set `ReadOnly` when it doesn't write (D-033).
- Third-party tools arrive through `MCP_SERVER_<NAME>_*`, not code. Don't relax trust: untrusted servers get no scope, no prompt instructions, no verbatim/await, and no subscriptions (D-038).
- A source that admits third-party text (like email) needs an allowlist, enforced in code.
- Whether an inbound message is for tobee is decided by the connector, and each rule names itself in the log so a dropped message can be explained (D-050). A channel restriction scopes group chatter, never DMs.

### Prompts & config

- Prompt text lives in `prompts/`, not in Go string literals. Existing exceptions:
  - protocol nudges and the `reply` / `plan` / `lessons` schemas in `loop.go`, `reply.go`, `plan.go`, `reflect.go`
  - the `<lessons>` framing in the memory server
  - tool `Description` fields
  - the tool menu the provider appends to each request (`internal/llm/openai/schema.go`)
  - the `<servers>` / `<context>` scaffolding in `context.go`
  - the workspace area list appended to its instructions
- Prompts are baked into the prod image (a rebuild ships changes). Dev compose bind-mounts them (a restart picks up changes).
- `.env` is the config surface. New variables go in `.env.example` with a one-line comment and are read in `cmd/tobee/main.go`. Anything the prod deploy needs also goes in `.env.prod.example` and the `Jenkinsfile`'s rendered env file.
- One instance, one wall clock: `TZ` pins `time.Local` at boot and `<context>` carries `tz`. Times a person reads are local; don't reintroduce `.UTC()` in user-facing text (D-049).
- TODO: The runtime image has no `USER` directive, so the container runs as root. Decide whether non-root is required.

### Git

- Commit subjects are imperative, sentence case, with no type prefix and no trailing period (e.g. `Let the planner answer directly; zero steps is the fast path`).
- Commit bodies explain the failure, the fix, and the cost, citing `D-0xx` IDs.
- AI-assisted commits end with a `Co-Authored-By:` trailer.
- Branches: the existing local branches use descriptive kebab-case (`synth-slim-context-violations`). TODO: Confirm the branch naming convention and which branch Jenkins deploys from.

### Documentation

- A design decision change adds a new `D-0xx` row in [DESIGN.md](DESIGN.md#key-decisions). Mark the old row superseded and log the change in [IMPLEMENTATION.md](IMPLEMENTATION.md). Never reuse an ID: code comments cite them. The next free ID is **D-062**.
- Routine code changes don't need doc edits. Update docs when shape, contracts, or config change.

### Working with the user

- Be terse. State decisions; don't narrate deliberation.
- Ask before destructive actions: deleting files, wiping `data/`, force-pushing, rewriting history.

## Knowledge Base Links

- [GOALS.md](GOALS.md): capabilities, audience, environment, non-goals, priorities.
- [DESIGN.md](DESIGN.md): architecture, turn lifecycle, reasoning patterns, memory model, key decisions, rejected alternatives.
- [IMPLEMENTATION.md](IMPLEMENTATION.md): history, milestones, superseded decisions, known limitations, technical debt.
- [GLOSSARY.md](GLOSSARY.md): project terms and protocols.
