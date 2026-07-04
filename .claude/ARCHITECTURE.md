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
├─ cmd/
│  └─ tobee/
│     └─ main.go                        # wiring only
├─ prompts/
│  ├─ persona/                          # identity persona fragments, concatenated in order
│  │  ├─ 00-identity.md
│  │  ├─ 01-tone.md
│  │  ├─ 02-behaviour.md
│  │  ├─ 03-output.md
│  │  └─ 04-safety.md
│  ├─ planner.md                        # planner phase role prompt
│  └─ synthesizer.md                    # synth phase role prompt
├─ internal/
│  ├─ abilities/
│  │  └─ reporter.go                    # Reporter contract + Registry (cross-subsystem introspection)
│  ├─ agent/
│  │  ├─ loop.go                        # serial worker; drives a turn, attaches scope to ctx
│  │  ├─ context.go                     # ContextBuilder (identity persona + memory hint + turn context)
│  │  ├─ planner.go, executor.go, synthesizer.go  # phases (D-024, D-025)
│  │  └─ reply.go                       # integration-name → ReplySender / MessageEditor / Reactor table
│  ├─ scope/
│  │  └─ scope.go                       # UserScope, ctx With/From, sanitized Key/Dir
│  ├─ llm/
│  │  ├─ client.go                      # OpenAI-compatible, native tool-use
│  │  └─ types.go                       # Message, ToolSpec, ToolCall
│  ├─ tools/
│  │  ├─ registry.go                    # JSON-Schema registry, timeouts, panic recovery
│  │  ├─ memory/                        # memory.{read,write,append,search,list} (scoped)
│  │  ├─ workspace/                     # workspace.{areas,list,read,write,search} over configured roots
│  │  ├─ schedule/                      # schedule.{create,cancel,list} — model-authored timers
│  │  └─ status/                        # status.summary / status.report — pre-rendered text from abilities.Reporters
│  ├─ integrations/
│  │  ├─ integration.go                 # interface + Envelope
│  │  ├─ bus.go                         # buffered channel event bus
│  │  └─ discord/                       # Discord gateway + reply sender + Reporter
│  ├─ sandboxfs/
│  │  └─ fs.go                          # typed sandboxed FS (backs memory + workspace)
│  ├─ workspace/
│  │  └─ areas.go                       # WORKSPACE_AREA_* env-driven area registry
│  └─ scheduler/
│     ├─ tick.go                        # static synthetic Envelopes + Reporter
│     ├─ jobs.go                        # dynamic, model-authored jobs (cron + one-shot)
│     ├─ jobstore.go                    # one JSON file per job under data/scheduler/jobs/
│     ├─ jobs_reporter.go               # schedules Reporter
│     └─ reporter.go                    # scheduler Reporter (static ticks)
├─ data/                                # gitignored; runtime state
│  ├─ memory/
│  │  ├─ shared/                        # cross-user knowledge
│  │  └─ users/<integration>/<userId>/  # per-user trees
│  └─ scheduler/
│     └─ jobs/<id>.json                 # one persisted job per file
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
5. ContextBuilder + agent loop (serial worker) — plan → announce → execute → synth → deliver.
6. Discord integration (gateway + message split + reply sender + reactor + message editor).
7. Scheduler skeleton (no static ticks registered).
8. Dynamic scheduled jobs: `schedule.*` tools + persistent JobManager.
9. `workspace.*` tool pack over configured host-file areas (D-019).
10. Strict tool-call protocol at every phase (D-025): plan.commit / step.finish / reply.commit.
11. Per-message turns — sessions and summariser retired (D-027).

What's not built, and why — see [DECISIONS.md](DECISIONS.md):

- Vector search, reflection cron, MCP, streaming replies, provider
  abstraction, archive rotation — deferred until there's a concrete need.
