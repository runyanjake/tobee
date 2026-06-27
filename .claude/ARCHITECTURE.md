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
│  ├─ agent/
│  │  ├─ loop.go                        # serial worker; drives a turn
│  │  ├─ context.go                     # ContextBuilder
│  │  ├─ session.go                     # short-term history ring buffer + SummaryStore
│  │  ├─ summarizer.go                  # post-turn rolling summary job
│  │  └─ reply.go                       # integration-name → ReplySender table
│  ├─ llm/
│  │  ├─ client.go                      # OpenAI-compatible, native tool-use
│  │  └─ types.go                       # Message, ToolSpec, ToolCall
│  ├─ tools/
│  │  ├─ registry.go                    # JSON-Schema registry, timeouts, panic recovery
│  │  └─ memory/                        # memory.{read,write,append,search,list}
│  ├─ integrations/
│  │  ├─ integration.go                 # interface + Envelope
│  │  ├─ bus.go                         # buffered channel event bus
│  │  └─ discord/                       # Discord gateway + reply sender
│  ├─ memory/
│  │  ├─ fs.go                          # sandboxed FS rooted at data/memory
│  │  └─ index.go                       # INDEX.md helper
│  └─ scheduler/
│     └─ tick.go                        # cron-like synthetic Envelopes (idle day one)
├─ data/                                # gitignored; runtime state
│  ├─ memory/                           # persistent memory
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
8. Scheduler skeleton (no ticks registered).

What's not built, and why — see [DECISIONS.md](DECISIONS.md):

- Vector search, reflection cron, MCP, streaming replies, provider
  abstraction, archive rotation — deferred until there's a concrete need.
