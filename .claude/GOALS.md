# Goals

## Core Capabilities

- **Take input from many sources.** Discord messages, email to an agent-owned inbox, its own scheduled jobs, and change notifications from trusted MCP servers all arrive as events in one durable task queue (D-034). Sources can be added or removed without touching the agent.
- **Chat through Discord.** Responds when mentioned (`@tobee` or a raw `<@id>`), replied to, or named as a whole word ("tobee, …"). Works in DMs and guild channels, and needs to be addressed in both. `DISCORD_CHANNEL_ID` and `DISCORD_ALLOWED_USERS` narrow it.
- **Handle email.** Polls an IMAP inbox for mail from allowlisted senders and replies in-thread over SMTP.
- **Plan and act with tools.** Commits a plan, announces it as a live checklist where the channel supports edits, executes steps with tools, and replies. Simple messages skip straight to a one-call direct reply (D-032).
- **Use any MCP server.** Built-in tools are MCP servers; external servers connect over stdio or HTTP with env config only. The model sees one catalog (D-033). External servers are untrusted by default (D-038).
- **Ask when unsure.** `user_ask` sends a clarifying question and pauses the task until the user answers (D-036).
- **Remember across messages.** Reads and writes plain-text memory files, split into a per-user tree and a shared tree (`memory_*`).
- **Schedule its own follow-ups.** Creates one-shot (`at`) or recurring (`cron`) jobs. They survive restarts and fire back into the channel that created them (`schedule_*`).
- **Report its own state.** `status_summary` and `status_report` return fixed, pre-formatted text about connectors, sources, the queue, MCP servers, and schedules, delivered word for word.
- **Work with host files (optional).** Lists, reads, searches, and writes (unless read-only) inside operator-configured workspace areas (`workspace_*`).
- **Swap the model by config.** Any OpenAI-compatible backend with tool calling, local or hosted (D-039). The reasoning strategy is pluggable (D-037).

## Supported Use Cases

- Q&A and chit-chat answered directly by the planner.
- Remembering user preferences, facts, and corrections, then recalling them later.
- Reminders, periodic check-ins, and deferred tasks ("check on this in an hour").
- Emailing tobee a task and getting the answer back in the same thread.
- Reacting to changes an MCP server reports (a new file, an alert) and posting the result to a channel.
- Multi-turn tasks that need one clarification before they can proceed.
- "What are you up to?" / "what's scheduled?" questions.
- Reading, searching, and drafting notes in configured host directories.

## Target Audience

- **Operator:** the repo owner, who self-hosts one instance and edits prompts, env, and `data/` directly.
- **End users:** Discord users who address the bot, and allowlisted email senders. The memory layout supports many users per instance (D-013). Scale is personal: one serial worker, no multi-tenant isolation beyond per-user memory trees.
- TODO: State the intended user population explicitly (owner only, family/friends server, or broader).

## Operational Environment

- **Prod:** one Linux host with an Nvidia GPU, deployed by Jenkins via `docker-compose.prod.yml`:
  - containers: `ollama`, a one-shot `ollama-pull`, and `tobee`
  - memory and jobs persist at the host path `/pwspool/software/tobee` (mounted at `/app/data`)
  - models persist in the `ollama-models` volume
- **Dev:** Docker Compose (`docker-compose.yml`) or `go run ./cmd/tobee`, pointed at LM Studio on the developer's machine.
- **External dependencies:**
  - Discord gateway and REST API (the bot needs the privileged Message Content intent)
  - an OpenAI-compatible LLM that supports native tool calling (local Ollama in prod today)
  - optional: an IMAP/SMTP mailbox; external MCP servers
- **No inbound ports.** tobee exposes no HTTP server or healthcheck. CI checks health by container status and the `tobee is running` log line.

## Non-Goals / Out of Scope

Deferred until there is a concrete need (D-004 and later entries):

- Vector or semantic search over memory. Substring search plus `INDEX.md` is the recall model.
- Background reflection or memory-consolidation passes.
- Exposing tobee itself as an MCP server. tobee is an MCP host only.
- Streaming or progressive replies. Progress is shown through plan-message edits and reactions.
- A provider abstraction beyond the OpenAI-compatible API. Hosted backends work through it (D-039); native non-OpenAI APIs need a proxy.
- Discord embeds or other structured reply channels (D-030).

Explicitly rejected:

- Chat history or a rolling summarizer across turns (D-027).
- A separate triage or classifier LLM call before planning (D-022 → D-023, D-032).
- Parsing tool calls the model wrote as text (D-025, `3e818f9`).
- `workspace.delete`, `workspace.move`, `workspace.exec`. Adding any of these needs a new decision (D-019).
- Prefix commands like `!memory.list` as a second control plane (D-007).
- Parallel task processing, parallel (DAG) plan steps, and persisting a plan or transcript across restarts (D-005, D-020, D-036). A parked task keeps only the request and the question.

## Current Operational Priorities

From the open questions in the former decision log and the latest commits (2026-07-19):

1. **Diagnose protocol violations.** The model sometimes returns `finish="stop"` with the tool call written as text, even with `tool_choice="required"`. Temperature now defaults to `0.1`; whether that fixes it is unmeasured. Next steps:
   - count violations at 0.1
   - confirm `tool_choice` is honored by the deployed model and server
   - check whether grammar-constrained output is available
2. **Decide on a smaller synthesizer context.** The unmerged branch `synth-slim-context-violations` builds the synthesizer's input as `[system, user request, directive]` instead of the full transcript.
3. **Watch the direct-reply fast path** for wrong answers the model should have looked up (D-032).
4. **Validate the MCP platform in prod** (2026-09-28). Confirm `qwen2.5:7b` handles the renamed tools and `user_ask`. Test the email connector against a real mailbox. Watch tool-choice accuracy as external servers are added.

TODO: Confirm these priorities and add any roadmap items not recorded in the repo.
