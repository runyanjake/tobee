# Goals

## Core Capabilities

- **Take input from many sources.** Discord messages, email to an agent-owned inbox, its own scheduled jobs, and change notifications from trusted MCP servers all arrive as events in one durable task queue (D-034). Sources can be added or removed without touching the agent.
- **Chat through Discord.** Answers every DM, and in a guild when mentioned (user or a role it holds), replied to, named as a whole word ("tobee, …"), or in a thread it is already in (D-050). `DISCORD_CHANNEL_ID` narrows which guild channel it listens to (its threads included); `DISCORD_ALLOWED_USERS` narrows who it answers.
- **Handle email.** Polls an IMAP inbox for mail from allowlisted senders and replies in-thread over SMTP.
- **Act with tools in one loop.** Calls one tool at a time until it can reply. Greetings and small talk are one call. Multi-step work gets a live checklist where the channel supports edits (D-043).
- **Use any MCP server.** Built-in tools are MCP servers; external servers connect over stdio or HTTP with env config only. The model sees one catalog (D-033). External servers are untrusted by default (D-038).
- **Ask when unsure.** `user_ask` sends a clarifying question and pauses the task until the user answers (D-036).
- **Remember across messages.** Reads and writes plain-text memory files, split into a per-person tree and a shared tree (`memory_*`).
- **Carry the conversation.** Each person has one live session across connectors, injected as history until it goes idle, then archived to memory as a transcript (D-045, D-046).
- **Schedule its own follow-ups.** Creates one-shot (`at`) or recurring (`cron`) jobs. They survive restarts and fire back into the channel that created them (`schedule_*`).
- **Report its own state.** `status_summary` and `status_report` return fixed, pre-formatted text about connectors, sources, the queue, MCP servers, and schedules, delivered word for word.
- **Work with host files (optional).** Lists, reads, searches, and writes (unless read-only) inside operator-configured workspace areas (`workspace_*`).
- **Keep one wall clock.** `TZ` sets the zone the instance thinks in, so "remind me at 4:40pm" and every time it reports back mean the same thing (D-049).
- **Swap the model by config.** Any OpenAI-compatible backend with tool calling, local or hosted (D-039). The reasoning strategy is pluggable (D-037).

## Supported Use Cases

- Q&A and chit-chat answered on the first model call, by calling `reply` (D-043).
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
  - an OpenAI-compatible LLM that supports structured output (local Ollama in prod today)
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

- A model-written rolling summary as conversation history (D-027); sessions record what happened instead (D-046).
- A separate triage or classifier LLM call before acting (D-022 → D-023).
- Fixed plan / execute / synthesize phases (D-024 → D-043).
- Parsing tool calls the model wrote as text (D-025, `3e818f9`).
- `workspace.delete`, `workspace.move`, `workspace.exec`. Adding any of these needs a new decision (D-019).
- Prefix commands like `!memory.list` as a second control plane (D-007).
- Parallel task processing, parallel (DAG) plan steps, and persisting a plan or transcript across restarts (D-005, D-020, D-036). A parked task keeps only the request and the question.

## Current Operational Priorities

From the open questions in the former decision log and the latest commits (2026-09-29):

1. **Confirm structured output in prod** (D-041). The cause of the text-written tool calls was found on 2026-09-28: Ollama's OpenAI endpoint ignores `tool_choice`. Every call now uses a JSON-schema `response_format`, which Ollama enforces by grammar. Verify on the prod Ollama and `qwen2.5:7b` that `agent: PROTOCOL VIOLATION` no longer appears and that tool choice is sensible.
2. **Watch the agent loop on the prod model** (D-043): does `qwen2.5:7b` reply directly to chit-chat, look things up before answering, and call `plan` only for real multi-step work?
3. **Validate the MCP platform in prod** (2026-09-28). Confirm `qwen2.5:7b` handles the renamed tools and `user_ask`. Test the email connector against a real mailbox. Watch tool-choice accuracy as external servers are added.

TODO: Confirm these priorities and add any roadmap items not recorded in the repo.
