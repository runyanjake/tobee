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
| 2026-09-28 | `e9b81ea` … `ab78cd9` (on `main`) | MCP platform (D-033 … D-039). Tool packs → in-process MCP servers behind `mcphost.Host`; external servers over stdio / HTTP. Bus and `Integration` → ingest engine plus a durable task queue. Discord becomes a connector; email connector added. `user_ask` with parked tasks. `Strategy` interface. LLM backend fully env-configured. Tool names `<server>_<tool>`. Categorized logging of the reasoning chain (D-040): model reasoning and token usage parsed, tool calls and results logged, incremental prompt logs, `LOG_FORMAT`, `LOG_CONTENT_LIMIT`. Prompts cut to about a quarter of their size, with every `tool({args})` example removed. `llm.Model` interface; the OpenAI-compatible provider uses schema-constrained structured output instead of `tool_choice`, which Ollama ignores (D-041). MCP resources: memory and workspace files as `memory://` / `workspace://` URIs read through `resources_read`; the system prompt comes from pinned resources (D-042). Fixed a cross-user path traversal in the memory tools. Fixed plan/execute/synthesize phases replaced by one agent loop with `reply` and `plan` tools (D-043); tool categories from MCP annotations (D-044); send tools refuse the current conversation (a greeting had been answered twice). Per-person sessions across connectors (D-045, D-046), code-enforced grounding with action lines and approvals, `memory_delete` (D-047), no more date-stamped filenames (D-048). |

| 2026-09-29 | (working tree) | Everything code writes to a person put in the first person and cut to activity: Reporter summaries, the failure block, the `[reminder due: …]` framing and a timer branch in the turn directive (D-053). Two-tier long-term memory replacing D-026: a capped, pinned `lessons.md` written by a reflection pass over closed sessions that failed, gated by `AGENT_REFLECT` (D-052). Failure reporting and the Reflexion record: `Turn.Problems`, the "⚠️ Didn't finish cleanly" block, repeated calls closing their tool, and `session.Outcome` carried into later turns and the transcript (D-051). One wall clock from `TZ`, stamped in `<context>` and used for every user-facing time (D-049). Discord addressing rewritten: DMs, role pings, and joined threads are addressed, each rule logged as `addressed_by`, and `DISCORD_CHANNEL_ID` no longer scopes out DMs or threads of that channel (D-050). |

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
  10. one tool-calling agent loop with `reply` and `plan` as tools (D-043)
- **Conversation state:** rolling summary, then saved ring buffer (D-016), then none (D-027), then none except parked questions (D-036), then one code-recorded session per person (D-045, D-046).
- **Tools:** `tools.Registry` plus Go tool packs, then MCP servers behind `mcphost.Host` (D-033). `internal/tools/{memory,workspace,schedule,status}` → `internal/servers/*`; `internal/tools/datedname` → `internal/datedname` → deleted with D-048. `prompts/system/05-tools.md` catalogue → `prompts/servers/<name>.md` instructions.
- **Input and output:** `integrations.Bus` (drop-on-full channel) and `Envelope` → `ingest.Engine`, `taskqueue.Queue`, and `event.Event` (D-034). `agent.Replies` → `delivery.Router` (D-035). `internal/integrations/discord` → `internal/connectors/discord`. The idle static tick `Scheduler` was deleted; `JobManager` is an ingest source.
- **Memory access:** always-injected INDEX / profile / preferences, then tool-only recall (D-026), then resource reads by URI (D-042), then two tiers — tool-read facts plus one pinned, capped `lessons.md` (D-052).
- **Security fix (2026-09-28):** memory tools joined the model's path onto the scope directory and relied on `sandboxfs`, which only confines to the whole memory tree. `../<other-user>/…` could read or overwrite another user's files. Paths are now confined to the scope root. A test covers read, write, append, and list.
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
| D-026 | Memory is never pre-loaded into the prompt and never pinned; recall is always a read | D-052. Removed from the active table: facts stay tool-only, but a capped `lessons.md` is pinned. |
| D-031 | Salvage parser for tool calls written as text | Reverted in `3e818f9`; entry removed from the log |
| D-024 | Tool-using turns run plan → announce → execute → synthesize | D-043 |
| D-032 | Planner may commit `direct_reply` with zero steps | D-043. Replying directly is just calling `reply` first. |
| D-029 (partial) | Per-phase state templates (`plan`, `execute_step`, `synthesize`) | D-043. One `turn` directive. |
| D-027 | Each message is a standalone turn; no history | D-046 |
| D-001 (partial) | Native OpenAI `tools` with `tool_choice=required` | D-041. Calls are still structured, now via `response_format: json_schema`. |
| D-004 | No vector search, reflection cron, or MCP | MCP: D-033. Vector search and reflection remain non-goals in [GOALS.md](GOALS.md#non-goals--out-of-scope). |
| D-009 | Replies go through the `Replies` table, not a `send.*` tool | D-035. The reply to the origin is still code; other sends are tools. |
| D-012 (partial) | Tools catalogue is the last system-prompt fragment | D-033. Fragment ordering stands. |
| D-012 (partial) | `main.go` reads and joins `prompts/system/*.md` | D-042. Now pinned resources of the `system` server. |
| D-019 (partial) | Workspace area list injected by `ContextBuilder` | D-033. Now part of the workspace server's instructions. |
| D-027 (partial) | Absolutely no state across turns | D-036. Parked questions only. |
| D-028 | Static hand-written `05-tools.md` catalogue | D-033. Server instructions plus `tools/list`. |
| D-030 (partial) | `tools.Spec.Verbatim` registry flag | D-033. Now `tobee/verbatim` tool metadata. |

## Known Limitations

- **Protocol violations were `tool_choice` being ignored.** Found 2026-09-28: Ollama's OpenAI endpoint has no `tool_choice` field, so `required` was never enforced. D-041 replaces it with constrained decoding. Checked against LM Studio (Qwen3 27B: a greeting got a direct reply, a lookup made a real tool call, zero violations). Prod Ollama with `qwen2.5:7b` is not yet verified; see [GOALS.md](GOALS.md#current-operational-priorities).
- **Premature replies.** The model can call `reply` from its own knowledge instead of looking something up. Only the `turn` directive guards against this (D-043).
- **Tool choice is still the model's.** "What reminders do I have?" went to `status_summary` instead of `schedule_list` on 2026-09-29; the descriptions now say which is which, but nothing enforces it, and a verbatim tool's output cannot be dropped once called (D-030, D-053).
- **A live session can steer a fired reminder wrong.** The creation exchange sits in history, so the model's own confirmation is the nearest precedent when the reminder comes due. The timer branch in the directive counters it by instruction only (D-053).
- **A false reply is contradicted, not prevented.** Code appends what went wrong (D-051), but the model's own sentence can still be wrong — as in the 2026-09-29 "I couldn't find any reminders" turn. The warning block is what tells the user to distrust it.
- **Nothing makes the model choose the right tool.** In that same turn `schedule_cancel` was offered, needed no approval, and was never called. Closing a repeated tool ends the loop sooner but doesn't point at the tool that would have worked.
- **A wrong lesson is sticky.** A lesson the reflection pass gets wrong sits in every prompt for that person until it ages past the 1 KiB cap or a human edits `lessons.md` (D-052). Nothing validates a lesson against later turns.
- **Reflection depends on a session closing.** Lessons are drawn at archive time, so a conversation that stays active never produces any, and a crash before the idle sweep loses them.
- **Lessons don't transfer between people** (D-052), so a second user re-learns the same thing.
- **Planning is optional.** The model decides whether to call `plan`; a small model may skip it on multi-step work.
- **Serial throughput.** One turn at a time, up to 2m each. A full queue (256) rejects new events with an ERROR log, and the sender is not notified.
- **At-least-once tasks.** A crash mid-turn replays the task on boot, which can repeat a reply or a side effect. A task is dropped after 2 attempts.
- **Session history is bounded.** 16 KiB of the newest exchanges; a 10-minute gap ends the conversation by design (D-046).
- **Accounts must be linked by the operator** (`IDENTITY_<NAME>`) for a person to be recognized across connectors.
- **Old memory files keep their date-stamped names** from before D-048 (e.g. `2026.07.20-index.md`); nothing renames them.
- **Answer matching is heuristic (D-036).** Without an explicit reply, the user's next message in that channel within 24h resumes the most recent question, even if it was about something else.
- **Everything runs on one zone.** `TZ` is the instance's clock (D-049). A user in another zone gets the operator's wall clock, and a `TZ` change needs a restart. Logs are local time, so correlating them with UTC-stamped external systems means converting.
- **Every DM is a turn** (D-050). With `DISCORD_ALLOWED_USERS` empty, anyone who can DM the bot can spend a turn; the allowlist is the only gate.
- **Every message in a joined thread is a turn** (D-050). Once tobee posts in a thread it answers each following message there, even ones aimed at someone else.
- **Discord specifics:**
  - `Thread` is never set on outbound addresses, and `Send` ignores it; a thread works only because a message in one carries the thread's own channel ID.
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

- The reflection pass has no budget of its own: it uses the model's own `AI_TIMEOUT` and the root context, so a slow model delays the idle sweep.
- `Dockerfile` has no `USER` directive (runs as root).
- `Jenkinsfile` doesn't set `AI_TEMPERATURE`, `AI_API_KEY`, or any email or MCP variables; the defaults apply.

### Dead code

- `StateData` is an empty struct: the one `turn` template takes no data, but the render path still threads it through.
- Two stale comments: `agent.Config` says "step caps live on Executor" (there is no `Executor`; `maxSteps` is on `Loop`), and `scope.UserScope.Key` carries two doc comments, the first describing the pre-D-045 `<connector>/<user>` key.

### Test coverage

Tests cover:

- `internal/agent`: context builder, `renderReply`, problem rendering and recovery, and end-to-end runtime tests against a scripted `llm.Model` (ask → park → resume; approval granted and declined; verbatim delivery; action lines; the logged chain; unreadable output dropped; a repeated read closing its tool; outcomes carried into the next turn and absent on a clean one)
- `internal/llm/openai`: request shape (structured output, no `tools` / `tool_choice`), schema building, ordering, and sanitizing, the tool menu, invalid-output rejection, native-call acceptance
- `internal/mcphost`: catalog, results, trust gating, server config, pinned resources
- `internal/taskqueue`: persistence, poison tasks, park/resume, expiry, capacity
- `internal/ingest`: dedup, allowlists, restart, runtime registration
- `internal/identity` and `internal/session`: account linking; history bounds, idle archive, transcripts
- `internal/telemetry`: categories, correlation, content limits
- `internal/connectors/email`: body parsing, threading headers, outbound allowlist
- `internal/connectors/discord`: every addressing rule that needs no gateway lookup, role-ping matching, DM scoping
- `internal/scheduler`: local wall-clock formatting of fire times
- `internal/servers/{memory,status,system}`: memory path confinement and archiving, lessons pinning per user with caps, status rendering, pinned prompt resources
- `internal/agent` reflection: lessons drawn from recorded failures, clean sessions skipped, empty and unreadable answers dropped, disabled is safe
- `internal/agent`: the timer branch of the turn directive, present for a fired reminder and absent for a plain message
- `internal/abilities`: summary joins activity only and stays first-person; the report still lists every reporter

No tests cover:

- `sandboxfs` (the security boundary), directly
- the workspace and schedule servers
- `scheduler` (job replay, misfire)
- the Discord split, mention rewriting, and the two lookups behind the role and thread rules (both need a live gateway)
- IMAP polling and SMTP sending against a live server
- external servers over Streamable HTTP

## Active / Unmerged Work

- **`synth-slim-context-violations`** (obsolete since D-043: there is no synthesizer) (local, `9b07674`, 2026-07-19, not merged):
  - The synthesizer builds its own `[system, user request, directive]` messages.
  - Protocol violations become countable.
  - Addresses the "synth re-emits `step.finish.result` as text" failure.
- **`simple-continuation-fast-path`** (local, `9c3cb68`): same content as `1f2f5c5` on `main`. Safe to delete after confirming.
- TODO: Delete the local `synth-slim-context-violations` branch; its synthesizer no longer exists.
