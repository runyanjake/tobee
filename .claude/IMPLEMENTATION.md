# Implementation

Built from `git log` (2026-03-05 → 2026-09-28) and the former `.claude/DECISIONS.md` log. Full commit bodies carry the detailed reasoning: `git log --format='%h %s%n%b'`.

## Evolution Timeline

| Date | Commits | Change |
|---|---|---|
| 2026-03-05 – 03-06 | `ed8dca2` … `30523ea` | Initial prototype: memory experiments, early agent loop, `context/*.md` prompts, `internal/ai`, `integrations/memory`. |
| 2026-04-21 | `511a714` | Large refactor into roughly today's shape: native tool-use client (D-001), `internal/` layout (D-002), sandboxed memory (D-003), serial worker (D-005), reply router (D-009). |
| 2026-04-22 – 05-12 | `08ff73e`, `eb48213` | Session janitor and idle rotation to `archive/` (D-010, D-011). Both removed later. |
| 2026-06-25 – 06-26 | `976c611`, `099438f` | Docker, `docker-compose.prod.yml` with GPU Ollama, Jenkins pipeline. |
| 2026-06-26 | `06eb684` … `19ba113` | Discord mention rewriting (both directions), addressed-only filter, persona fragments (D-012), multi-user memory plus Reporter contract (D-013, D-014), dynamic scheduled jobs (D-015). |
| 2026-06-27 | `6833ce0` … `ef2b312` | Saved session ring (D-016), prefix-cache ordering (D-017), `cmd/tobee` plus canonical module path (D-018), workspace areas and `sandboxfs` extraction (D-019), date-stamped filenames. |
| 2026-06-28 | `ae55a49` … `ce8e26e` | Prod data at `/pwspool/software/tobee`; plan / act / replan / synthesize loop (D-020); plan announcement; `status.summary` / `status.report` (D-021); `LOG_LEVEL`. |
| 2026-06-29 | `18aef2e` … `f4d128b` | Triage phase plus state-machine driver (D-022) → collapsed to act-loop plus synthesis (D-023) → restored plan → announce → execute → synthesize (D-024). Jenkins Discord notifications. Message Content intent. |
| 2026-07-02 | `d1a3864`, `c0bb476` | Progress reactions; generated content in code blocks. |
| 2026-07-04 | `54e72ca` … `13ccc7a` | Strict tool-call protocol and no pre-loaded memory (D-025, D-026). LLM error retries. Per-message turns: sessions, summarizer, and janitor deleted (D-027). Prompts baked into the prod image. `prompts/persona` → `prompts/system` plus static tools catalogue (D-028). One `Conversation` per request with state templates (D-029). |
| 2026-07-19 | `a974c76` … `1f2f5c5` | User text split from `<phase>` directives. Verbatim enforced in code, clock stamp, relative status `window` (D-030). Salvage parser added (D-031) then reverted. Temperature default 0.7 → 0.1, now configurable. Planner `direct_reply` fast path (D-032). |
| 2026-09-28 | branch `mcp-platform` | MCP platform (D-033 … D-039). Tool packs → in-process MCP servers behind `mcphost.Host`; external servers over stdio / HTTP. Bus and `Integration` → ingest engine plus a durable task queue. Discord becomes a connector; email connector added. `user_ask` with parked tasks. `Strategy` interface. LLM backend fully env-configured. Tool names `<server>_<tool>`. Categorized logging of the reasoning chain (D-040): model reasoning and token usage parsed, tool calls and results logged, incremental prompt logs, `LOG_FORMAT`, `LOG_CONTENT_LIMIT`. |

## Major Refactors & Migrations

- **How the agent loop's shape changed:**
  1. single ReAct loop (D-001)
  2. plan / act / replan / synthesize (D-020)
  3. triage plus state machine (D-022)
  4. act-loop plus synthesis (D-023)
  5. plan → announce → execute → synthesize (D-024)
  6. strict protocol (D-025)
  7. one conversation (D-029)
  8. direct-reply fast path (D-032)
  9. serial runtime over a task queue, with `PlanExecute` as one `Strategy` (D-034, D-037)
- **Conversation state:** rolling summary, then saved ring buffer (D-016), then none (D-027), then none except parked questions (D-036).
- **Tools:** `tools.Registry` plus Go tool packs, then MCP servers behind `mcphost.Host` (D-033). `internal/tools/{memory,workspace,schedule,status}` → `internal/servers/*`; `internal/tools/datedname` → `internal/datedname`. `prompts/system/05-tools.md` catalogue → `prompts/servers/<name>.md` instructions.
- **Input and output:** `integrations.Bus` (drop-on-full channel) and `Envelope` → `ingest.Engine`, `taskqueue.Queue`, and `event.Event` (D-034). `agent.Replies` → `delivery.Router` (D-035). `internal/integrations/discord` → `internal/connectors/discord`. The idle static tick `Scheduler` was deleted; `JobManager` is an ingest source.
- **Memory access:** always-injected INDEX / profile / preferences, then tool-only recall (D-026).
- **Prompt files:** `context/{PROMPT,SOUL,TOOLS}.md` → `prompts/persona.md` (D-006) → `prompts/personality/` → `prompts/persona/` (D-012, D-018) → `prompts/system/` plus `prompts/state/` (D-028, D-029). `prompts/planner.md`, `synthesizer.md`, `triage.md`, `summarizer.md` were deleted.
- **Filesystem:** `internal/memory` FS extracted to `internal/sandboxfs` and shared with workspace areas (D-019).
- **Layout:** root `main.go` → `cmd/tobee/main.go`; module `tobee` → `github.com/runyanjake/tobee` (D-018).
- **Deploy:** prompts bind-mounted in prod → baked into the image (`a9ed577`). Data moved to the host path `/pwspool/software/tobee` (`e9ac2d7`).
- **Docs (2026-09-15):** `.claude/{README,ARCHITECTURE,AGENT,MEMORY,INTEGRATIONS,DECISIONS,CONVENTIONS}.md` and root `CLAUDE.md` consolidated into `.claude/{CLAUDE,GOALS,DESIGN,IMPLEMENTATION,GLOSSARY}.md`. Earlier decision text is in git history (`git show 1f2f5c5:.claude/DECISIONS.md`).

## Superseded Decisions

| ID | Was | Superseded by |
|---|---|---|
| D-006 | Single `prompts/persona.md` | D-012 |
| D-010 | In-process janitor pruning `data/sessions/` | D-027 (janitor deleted) |
| D-011 | Idle session rotation to `archive/` | D-027 |
| D-016 | Saved session ring (`recent.json`), DM-aware idle timeouts | D-027 |
| D-017 (partial) | Memory and session sections in the system-prompt order | D-026, D-027. Stable-prefix rule stands. |
| D-020 | Plan / act / replan / synthesize with `plan.revise` | D-023, then D-024 (no replanning) |
| D-022 | Triage phase plus `phaseFn` state machine | D-023 |
| D-023 | Unified act-loop plus always-on synthesis | D-024 |
| D-024 (partial) | Text-wrap planner fallback; synthesis given only the plan; per-step tool scopes; mandatory steps for every turn | D-025, D-029, D-029, D-032 |
| D-026 (partial) | Session summary still pre-loaded | D-027 |
| D-031 | Salvage parser for tool calls written as text | Reverted in `3e818f9`; entry removed from the log |
| D-004 | No vector search, reflection cron, or MCP | MCP: D-033. Vector search and reflection remain non-goals in [GOALS.md](GOALS.md#non-goals--out-of-scope). |
| D-009 | Replies go through the `Replies` table, not a `send.*` tool | D-035. The reply to the origin is still code; other sends are tools. |
| D-012 (partial) | Tools catalogue is the last system-prompt fragment | D-033. Fragment ordering stands. |
| D-019 (partial) | Workspace area list injected by `ContextBuilder` | D-033. Now part of the workspace server's instructions. |
| D-027 (partial) | Absolutely no state across turns | D-036. Parked questions only. |
| D-028 | Static hand-written `05-tools.md` catalogue | D-033. Server instructions plus `tools/list`. |
| D-030 (partial) | `tools.Spec.Verbatim` registry flag | D-033. Now `tobee/verbatim` tool metadata. |

## Known Limitations

- **Protocol violations from the local model.** Tool calls sometimes come back as text under `tool_choice=required`. Root cause undiagnosed; see [GOALS.md](GOALS.md#current-operational-priorities).
- **Synthesis continuation risk.** Synthesis runs on the full transcript, so the model may keep talking instead of presenting results. It's held back by `synthesize.md` wording plus the forced `reply_commit`.
- **Date stamping conflicts with canonical memory files.**
  - `datedname.Apply` rewrites every `memory_write` / `memory_append` / `workspace_write` path to `YYYY.MM.DD-<kebab>.<ext>`.
  - The prompts tell the model to read and maintain `INDEX.md`, `user.md`, and `preferences.md`. The tools can never create or update those exact names.
  - Appends on a later day go to a new file.
  - A name that already has a date gets a second one.
- **Direct-reply misroutes.** The planner can answer from its own knowledge instead of looking something up. Only prompt wording guards against this (D-032).
- **Serial throughput.** One turn at a time, up to 2m each. A full queue (256) rejects new events with an ERROR log, and the sender is not notified.
- **At-least-once tasks.** A crash mid-turn replays the task on boot, which can repeat a reply or a side effect. A task is dropped after 2 attempts.
- **No cross-turn context** beyond parked questions. Follow-ups that depend on the previous message fail unless the model saved something to memory (D-027).
- **Answer matching is heuristic (D-036).** Without an explicit reply, the user's next message in that channel within 24h resumes the most recent question, even if it was about something else.
- **Identity is per connector.** The same person on Discord and email has two memory trees and cannot answer a question across connectors.
- **Discord specifics:**
  - `Thread` is never set, and `Send` ignores it.
  - Multi-chunk replies return only the last chunk's ID. Plan-message edits assume the announcement fits in one chunk.
  - Requires the privileged Message Content intent.
  - Answers to `user_ask` must be addressed (reply, mention, or name) like any message.
  - A bad token no longer exits the process; the source retries with backoff. CI checks `discord: connected`.
- **Email specifics:** HTML-only mail is skipped. Attachments are ignored. Subjects for reply threading are cached in memory, so replies after a restart use a generic subject.
- **External stdio servers** run inside the runtime image, which has no Node or Python.
- **External server tool lists** refresh on `list_changed`, but a server that drops is not reconnected until restart.
- **Timer events** carry no `MessageID`, so they get no progress reactions.
- **Hardcoded limits:** queue capacity (256), parked TTL (24h), task attempts (2), tool result cap (32 KiB), memory file cap (64 KiB).
- **Logs hold user content.** INFO records carry messages, tool results, and replies up to `LOG_CONTENT_LIMIT` bytes each. Anyone with log access can read them.
- **Reasoning capture depends on the server.** Only servers that return `reasoning_content` or `reasoning` separately are captured as `agent: reasoning`; a model that writes `<think>` inside its content shows up as `agent: model text`.
- **No container health endpoint.** Prod health is inferred from container state and boot log lines.

## Technical Debt

### Config and deploy drift

- `Dockerfile` has no `USER` directive (runs as root).
- `Jenkinsfile` doesn't set `AI_TEMPERATURE`, `AI_API_KEY`, or any email or MCP variables; the defaults apply.

### Dead code

- `Plan.Next`, `Plan.Render`, and `sandboxfs.FS.Exists` have no callers.
- `Conversation.SurfacedKnowledge` and `StateData.SurfacedKnowledge` are placeholders for a future web/file search integration and are never filled in.

### Test coverage

Tests cover:

- `internal/agent`: context builder, planner commit parsing, `renderReply`, and an end-to-end runtime test with a scripted LLM (ask → park → resume; verbatim delivery)
- `internal/mcphost`: catalog, results, trust gating, server config
- `internal/taskqueue`: persistence, poison tasks, park/resume, expiry, capacity
- `internal/ingest`: dedup, allowlists, restart, runtime registration
- `internal/connectors/email`: body parsing, threading headers, outbound allowlist
- `internal/datedname`, `internal/servers/status`

No tests cover:

- `sandboxfs` (the security boundary)
- the memory / workspace / schedule servers
- `scheduler` (job replay, misfire)
- the Discord connector (address filter, mention rewriting, split)
- IMAP polling and SMTP sending against a live server
- external servers over Streamable HTTP

## Active / Unmerged Work

- **`synth-slim-context-violations`** (local, `9b07674`, 2026-07-19, not merged):
  - The synthesizer builds its own `[system, user request, directive]` messages.
  - Protocol violations become countable.
  - Addresses the "synth re-emits `step.finish.result` as text" failure.
- **`simple-continuation-fast-path`** (local, `9c3cb68`): same content as `1f2f5c5` on `main`. Safe to delete after confirming.
- TODO: Decide whether to merge `synth-slim-context-violations`. It predates the MCP platform and needs a rebase (virtual tools are now `step_finish` / `reply_commit`). If merged, record the new synthesizer input contract as D-040.
