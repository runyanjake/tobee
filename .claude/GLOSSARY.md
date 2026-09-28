# Glossary

| Term | Definition |
|---|---|
| **Ability** | A capability implemented outside the LLM turn. Today: introspection through `internal/abilities`. |
| **Actor** | Who caused an event: `event.Actor{ID, Name}`. Empty for notifications. The ID is per connector (Discord user ID, email address). |
| **Addressed message** | A Discord message tobee will process: a bot mention, raw `<@id>` or `<@!id>`, a reply to the bot, or the whole word `tobee`. Everything else is dropped as ambient chatter. |
| **Address** | Where output goes: `event.Address{Connector, Channel, Thread}`. An event's `Origin` is the address its reply is sent to. |
| **Allowlist** | Per-source list of admitted actor IDs (`ingest.Engine.Allow`). `DISCORD_ALLOWED_USERS` (optional), `EMAIL_ALLOWED` (required; also limits outbound mail). |
| **Area** | See *Workspace area*. |
| **Artifact** | A `{lang, body}` item in `reply`: content handed over rather than said (code, drafts, snippets). Delivered as a fenced block. |
| **Await** | `mcpserver.Await{Question, Keys}`, returned by a trusted tool in `_meta["tobee/await"]` to park the task. Set by `user_ask`. |
| **Budget** | A hard cap on a turn: wall-clock (`AGENT_TURN_BUDGET`, 2m) and model calls (`AGENT_MAX_STEPS`, 12). |
| **Built-in server** | An MCP server compiled into tobee (`memory`, `workspace`, `schedule`, `status`, `user`, plus each connector's server). Connected in-process and always trusted. |
| **Catalog** | The union of every connected server's tools, as the model sees them: `<server>_<tool>` names. Built by `mcphost.Host` from `tools/list`. |
| **Channel** | (1) A `delivery.Channel`: a connector's `Send`, plus optional `Editor` and `Reactor`. (2) A connector-specific conversation ID in an `Address` (Discord channel ID, email address). |
| **Connector** | A package for one external system that can be an ingest source, a delivery channel, and an MCP server at once: `connectors/discord`, `connectors/email`. |
| **`<context>` tag** | The system-message section stamping `now`, source, kind, connector, channel, thread, and user (or `user=none`) for the turn. |
| **Conversation** | The `agent.Conversation` for one task. A single growing `[]llm.Message` shared by all phases, plus the `Plan`. |
| **D-0xx** | A design decision ID. Current ones are in `DESIGN.md#key-decisions`, superseded ones in `IMPLEMENTATION.md`. Cited in code comments; never reused. |
| **Dated filename** | The `YYYY.MM.DD-kebab-name.ext` form that `datedname.Apply` gives every written memory or workspace file. |
| **Deliver** | The runtime's last step: send `Turn.Reply` to the event's origin, then clear reactions (success) or add ❌ (empty reply). |
| **Delivery router** | `delivery.Router`, the connector-name → `Channel` table the runtime and `user_ask` send through. |
| **Event** | `event.Event`, the normalized inbound unit every source emits: ID, source, kind, actor, origin, message ID, in-reply-to, content. |
| **External server** | An MCP server configured with `MCP_SERVER_<NAME>_*`, connected over stdio (`_COMMAND`) or Streamable HTTP (`_URL`). Untrusted unless `_TRUSTED=true`. |
| **Host (MCP host)** | `mcphost.Host`: holds one client session per MCP server, builds the catalog, routes tool calls, and enforces trust. |
| **INDEX.md** | The per-scope memory table of contents the prompts tell the model to read first. Meant to be maintained by a human. |
| **Ingest engine** | `ingest.Engine`: supervises sources, restarts them with backoff, dedups events, applies allowlists, and enqueues tasks. |
| **Instructions** | An MCP server's `instructions` string, shown to the model under `<servers>`. Built-in servers load theirs from `prompts/servers/<name>.md`. Hidden for untrusted servers. |
| **JobManager** | `scheduler.JobManager`, which owns model-created jobs (cron or one-shot), their JSON files, and emits timer events as the `schedule` source. |
| **`_meta`** | MCP's per-request/result metadata field. tobee uses `tobee/scope` (request), `tobee/verbatim` (tool definition), and `tobee/await` (result), only with trusted servers. |
| **Log category** | The `cat` attribute on every log record: `input`, `thinking`, `action`, `output`, `llm`, or `system` (D-040). |
| **Memory scope** | `user` (`data/memory/users/<connector>/<id>/`), `shared` (`data/memory/shared/`), or `both` (search and list only). |
| **Misfire policy: skip** | One-shot jobs whose time passed while tobee was down are deleted at boot, not run. |
| **Model (`llm.Model`)** | The agent's only interface to an LLM: `Decide` returns exactly one call to one offered tool (D-041). Implemented by the OpenAI-compatible provider in `internal/llm/openai`. |
| **Nudge** | The short "could not be read as a tool call" user message appended before the single retry of a phase. The unreadable output itself is dropped. |
| **Parked task** | A task that asked the user a question and is waiting for the answer in `data/tasks/parked/`. Holds the request, question, and resume keys; expires after 24h (D-036). |
| **Agent loop** | `agent.Loop`, the `react` strategy: one tool per model call until `reply`, a `user_ask`, or `AGENT_MAX_STEPS` (D-043). |
| **Category** | A tool's group in the model's menu: `read`, `write`, `external`, or `finish`, derived from MCP annotations (D-044). |
| **Phase directive / state template** | A user-role message rendered from `prompts/state/<phase>.md` and wrapped in `<phase name="…">`. Carries that phase's contract. |
| **Plan** | `agent.Plan`: `Goal`, ordered `Steps` (intent, status, result, error), or a `DirectReply`. Exists only for the turn. |
| **Prefix cache** | LLM-server KV cache reuse when consecutive requests share leading tokens. Why the system message keeps stable sections first and is never rebuilt mid-turn. |
| **Pinned resource** | A resource a trusted server marks priority 1. The host puts its text in every system prompt (D-042). Only the `system` server pins today. |
| **Protocol violation** | Model output that is not a valid choice of an offered tool (`llm.ErrInvalidDecision`). With constrained decoding it means the server ignored the schema. Logged at ERROR, dropped, nudged, retried once. |
| **Structured output** | A JSON Schema sent as `response_format` that the server enforces while decoding (a grammar on Ollama). How every model call is made (D-041). |
| **Tool menu** | The `<tools>` block the provider appends to each request, describing the offered tools. The schema constrains output, but the model never sees it. |
| **Resource** | Readable content a server exposes under a URI, e.g. `memory://user/INDEX.md`. The model reads resources with `resources_read`. |
| **Resource template** | A URI pattern for a family of resources, e.g. `memory://{scope}/{+path}` or `workspace://{area}/{+path}`. |
| **ReAct** | Reason + Act: alternate model tool calls and tool results. The executor's inner loop. |
| **Reaction lifecycle** | ✅ received → 🧠 planning → 💭 executing on the inbound message. Cleared on success or park; ❌ on failure. |
| **`reply`** | The loop's tool that answers and ends the turn: `{spoken, artifacts}`. |
| **`plan`** | The loop's checklist tool for multi-step work. Shown only with 2+ steps on connectors that can edit it. |
| **Reporter** | `abilities.Reporter`, a subsystem's `Render(ctx, since) → (full, summary)` text for status tools. Current reporters: `discord`, `email`, `ingest`, `mcp`, `schedules`, `tasks`. |
| **Resource source** | `mcphost.ResourceSource`: an ingest source that subscribes to a trusted server's resources and emits a notification event on each update. |
| **Resume keys** | Strings that match an answer to a parked task: `reply:<connector>:<channel>:<msgID>` (explicit reply) and `actor:<connector>:<channel>:<userID>` (fallback). |
| **Runtime** | `agent.Runtime`, the serial worker: dequeue a task, run the strategy, deliver or park (D-005). |
| **Sandbox** | `sandboxfs.FS`, a filesystem rooted at one directory that rejects absolute, volume-qualified, and `..` paths and enforces a size cap. |
| **Scheduled fire** | The timer event a job emits when it fires. Content `[scheduled fire: <name>] <prompt>`, routed to the creating channel and user. |
| **Scope (`UserScope`)** | The per-turn connector / user / user name / channel / thread, attached to `ctx` via `scope.With` and sent to trusted servers in `_meta`. |
| **Serial worker** | See *Runtime*. |
| **Server (MCP server)** | A provider of tools (and optionally resources) speaking MCP. Built-in or external. |
| **`<servers>` block** | The system-message section listing each connected server with its instructions and tools. |
| **Source** | An `ingest.Source`: anything with `Name()` and `Run(ctx, emit)` that produces events. `discord`, `email`, `schedule`, `mcp_<server>`. |
| **`spoken`** | The plain-text part of `reply`, in tobee's voice. |
| **Status tools** | `status_summary` and `status_report`. Verbatim tools rendering Reporter output over a relative `window`. |
| **Strategy** | An `agent.Strategy`: a reasoning scheme that turns a `Turn` into a reply or an await. The agent loop (`react`) is the only one (D-037). |
| **System prompt** | Pinned resources (`prompts/system/*.md` in filename order), plus `<servers>` and `<context>`. Sent once as message 0. |
| **Task** | `taskqueue.Task`: an event, a `Resume` if it answers a parked question, and an attempt count. Persisted until done. |
| **Task queue** | `taskqueue.Queue`, the durable FIFO between ingest and the runtime; also stores parked tasks. |
| **Trust** | Per-server setting. Trusted servers get scope, show instructions, and may set verbatim/await or feed tasks; untrusted ones may not (D-038). |
| **Turn** | `agent.Turn`, all processing for one task, from dequeue to deliver or park. |
| **Verbatim** | A tool marked `tobee/verbatim` by a trusted server: its output is appended to the reply by code and never rewritten by the model (D-030). |
| **Virtual tool** | A phase-only tool schema that no server provides: `reply`, `plan`. |
| **`window`** | The status tools' relative lookback (`30m`, `24h`, `7d`). Default 1h, max 30d. |
| **Workspace area** | An operator-configured sandboxed host directory (`WORKSPACE_AREA_<NAME>`, optional `_DESC`, `_READONLY`) reachable through `workspace_*`. |
