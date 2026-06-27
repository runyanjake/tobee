# Architecture

## What tobee is

A long-running Go service that:

1. Listens on one or more integrations (Discord today) for inbound messages.
2. Runs each message through an agentic loop that calls a local LLM
   (OpenAI-compatible, native tool-use) and may invoke tools mid-turn.
3. Replies via the originating integration.
4. Remembers things across restarts by reading and writing plain text files
   under `data/`.

One user, many integrations, one shared file-based memory.

## Three concerns, one process

```
                    ┌─────────────────────────────────────────┐
                    │              tobee process              │
                    │                                         │
 Discord ──┐        │  ┌────────────┐                         │
 Slack ────┼─►┌─────┴─►│ EventBus   │── Envelope ─┐           │
 Cron  ────┘  │       └────────────┘              ▼           │
              │                             ┌────────────┐    │
              │                             │ Agent Loop │    │
              │                             │  (1 goro)  │    │
              │                             └─────┬──────┘    │
              │                                   │           │
              │               ┌───────────────────┼──────┐    │
              │               ▼                   ▼      ▼    │
              │        ┌────────────┐     ┌─────────────┐     │
              │        │  Context   │     │  LLM Call   │     │
              │        │  Builder   │     │  (tool use) │     │
              │        └─────┬──────┘     └──────┬──────┘     │
              │              │                   │            │
              │              ▼                   ▼            │
              │        ┌────────────┐     ┌─────────────┐     │
              │        │  Memory    │     │    Tool     │     │
              │        │  (files)   │◄───►│  Registry   │◄────┘ (reply senders
              │        └────────────┘     └──────┬──────┘        registered here)
              │                                  │
              │                                  ▼
              │                          integration.Send(…)
              └──────────────────────────────────┘
```

- **Integrations** = producers of inbound events + ability to send outbound.
- **Agent Loop** = serial consumer; runs one envelope to completion before
  picking up the next.
- **Tools + Memory** = things the loop uses during a turn. Everything the
  model can *read*, *write*, or *do* flows through the tool registry.

See [AGENT.md](AGENT.md) for loop detail, [INTEGRATIONS.md](INTEGRATIONS.md)
for the integration/tool contracts, [MEMORY.md](MEMORY.md) for the memory
subsystem.

## Repository layout

```
tobee/
├─ main.go                              # wiring only
├─ prompts/
│  ├─ personality/                      # system prompt fragments, concatenated in order
│  │  ├─ 00-identity.md
│  │  ├─ 01-tone.md
│  │  ├─ 02-behaviour.md
│  │  ├─ 03-output.md
│  │  └─ 04-safety.md
│  └─ summarizer.md                     # prompt for the rolling summarizer
├─ internal/
│  ├─ abilities/
│  │  └─ reporter.go                    # Reporter contract + Registry (cross-subsystem introspection)
│  ├─ agent/
│  │  ├─ loop.go                        # serial worker; drives a turn, attaches scope to ctx
│  │  ├─ context.go                     # ContextBuilder (shared + per-user memory)
│  │  ├─ session.go                     # short-term history ring buffer + SummaryStore
│  │  ├─ summarizer.go                  # post-turn rolling summary job
│  │  └─ reply.go                       # integration-name → ReplySender table
│  ├─ scope/
│  │  └─ scope.go                       # UserScope, ctx With/From, sanitized Key/Dir
│  ├─ llm/
│  │  ├─ client.go                      # OpenAI-compatible, native tool-use
│  │  └─ types.go                       # Message, ToolSpec, ToolCall
│  ├─ tools/
│  │  ├─ registry.go                    # JSON-Schema registry, timeouts, panic recovery
│  │  ├─ memory/                        # memory.{read,write,append,search,list} (scoped)
│  │  ├─ schedule/                      # schedule.{create,cancel,list} — model-authored timers
│  │  └─ status/                        # status.report — aggregates abilities.Reporter snapshots
│  ├─ integrations/
│  │  ├─ integration.go                 # interface + Envelope
│  │  ├─ bus.go                         # buffered channel event bus
│  │  └─ discord/                       # Discord gateway + reply sender + Reporter
│  ├─ memory/
│  │  └─ fs.go                          # sandboxed FS rooted at data/memory
│  └─ scheduler/
│     ├─ tick.go                        # static synthetic Envelopes + Reporter
│     ├─ jobs.go                        # dynamic, model-authored jobs (cron + one-shot)
│     ├─ jobstore.go                    # one JSON file per job under data/scheduler/jobs/
│     ├─ jobs_reporter.go               # schedules Reporter
│     ├─ janitor.go                     # in-process session cleanup
│     ├─ reporter.go                    # scheduler Reporter (static ticks)
│     └─ janitor_reporter.go            # janitor Reporter
├─ data/                                # gitignored; runtime state
│  ├─ memory/
│  │  ├─ shared/                        # cross-user knowledge
│  │  └─ users/<integration>/<userId>/  # per-user trees
│  ├─ scheduler/
│  │  └─ jobs/<id>.json                 # one persisted job per file
│  └─ sessions/                         # rolling per-channel summaries
└─ CLAUDE.md, .claude/                  # this documentation
```

`data/` is the single source of truth for state that survives a restart.
No DB, no vector store, no SQLite.

## Implementation status

What's built (in order it was added):

1. Skeleton + module wiring.
2. LLM client speaking native OpenAI-compatible tool-use.
3. Tool registry with JSON-Schema, timeouts, panic recovery.
4. Memory FS (sandboxed) + `memory.*` tool pack.
5. ContextBuilder, SessionStore, agent loop (serial worker).
6. Post-turn rolling summarizer.
7. Discord integration (gateway + message split + reply sender).
8. Scheduler skeleton (no static ticks registered).
9. Dynamic scheduled jobs: `schedule.*` tools + persistent JobManager.

What's not built, and why — see [DECISIONS.md](DECISIONS.md):

- Vector search, reflection cron, MCP, streaming replies, provider
  abstraction, archive rotation — deferred until there's a concrete need.
