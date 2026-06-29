# Agent

The agent loop lives in [internal/agent/](../internal/agent/). One worker
goroutine consumes Envelopes from the bus and drives each through a
uniform **two-phase turn**: an act loop (the model freely calling tools
until it produces terminal text) followed by a synthesis LLM call that
composes the user-facing reply. The structure rides on the LLM's
`tool_calls` response — we never parse JSON out of the text body. See
[DECISIONS.md](DECISIONS.md) D-001 and D-023.

## Per-turn state: `Turn`

`Turn` ([internal/agent/turn.go](../internal/agent/turn.go)) is the
single value threaded through the turn's two phases:

| Field      | Set by                                       |
|------------|----------------------------------------------|
| Ctx        | processTurn (with turn timeout + scope)      |
| Env        | processTurn (from the envelope)              |
| Session    | processTurn (loaded from SessionStore)       |
| Transcript | ContextBuilder; grown by Executor.Run        |
| Reply      | Synthesizer.Finalize                         |

## Shape

```
Executor.Run (act loop)  ─►  Synthesizer.Finalize  ─►  deliver
```

`processTurn` is a linear sequence, not a state machine. No phaseFn
driver, no categorical routing, no Plan/Step artifact — the model
itself decides what (if any) tools to call inside the act loop.

## The phases

1. **Act loop** ([internal/agent/executor.go](../internal/agent/executor.go)).
   `Executor.Run` drives the per-turn ReAct loop, bounded by
   `LOOP_MAX_ITERATIONS` (default 12). Per iteration:
   - Compose system message: persona + data sections.
   - Call the LLM with every registered tool advertised.
   - Append the assistant message (text + tool_calls) to the
     in-flight transcript and the session.
   - Dispatch any tool_calls; append each result as a `role: tool`
     message.
   - Terminate when the response has no tool_calls. This includes
     the very first iteration — a greeting produces zero tool calls
     and ends the loop in one round-trip. Trivial input is the
     expected fast path, not a special case.
   The act loop is the model's scratchpad. Its terminal text is
   passed to the synthesiser, not directly to the user.

2. **Synthesis** ([internal/agent/synthesizer.go](../internal/agent/synthesizer.go)).
   Always runs. `Synthesizer.Finalize` makes one LLM call with the
   synthesiser persona ([prompts/synthesizer.md](../prompts/synthesizer.md))
   advertising no tools. It reads the act-loop transcript (tool calls,
   tool results, terminal text) and composes the one outbound message.
   Tone, formatting, and length are enforced here.

After synthesis, `deliver`:

1. Mirrors `turn.Reply` into the session (so the next turn's act loop
   sees what was said).
2. Sends it via `Replies.Send(integration, channel, …)`.
3. Runs the summarizer against the session transcript (best-effort).

The act-loop transcript is **turn-scoped** as an in-flight artifact,
but the messages within it are mirrored to the session as they go —
so a mid-turn crash leaves the user message + any tool calls already
made committed to `recent.json`.

## Key choices

- **Serial worker.** A single goroutine drains the bus. Makes
  memory-file writes race-free without locks, keeps replies
  deterministically ordered, avoids parallel-reply footguns. A
  message from channel A blocks a message from channel B for the
  duration of one turn. Acceptable for a personal agent. Revisit if
  scale demands.

- **Native tool-use, not JSON-in-string.** The LLM returns structured
  `tool_calls`; we never parse the text body for a JSON envelope.
  Load-bearing.

- **One shape for every turn.** Greetings, knowledge questions,
  status, action requests — all flow through act loop + synthesis.
  "No tool call needed" is the expected trivial path on iteration 1,
  not an error. See D-023 for why the previous triage/plan scaffold
  was collapsed.

- **Act loop is scratchpad; synthesiser is the user-facing voice.**
  The act loop emits assistant messages that may or may not be in
  tobee's voice; the synthesiser always rewrites them to tobee's
  voice with consistent tone and length. Pay one extra LLM call per
  turn for predictable formatting.

- **Budgets.** `Config.TurnBudget` (default 2 min, wall-clock) is the
  outer ceiling. `Config.MaxReplans` (default 3) caps replan calls.
  `Executor.maxPerStep` (default 4) caps each step's sub-loop. The
  total across the whole turn is capped at `Executor.totalBudget`
  (default 12).

- **Interruptible.** The turn takes a context derived from the service
  context; cancellation during shutdown stops the turn mid-phase.

- **Summarizer is best-effort.** Its failure must never block a
  reply.

## Context building

[internal/agent/context.go](../internal/agent/context.go) composes the
initial message list for a turn. Sections, in fixed order:

| # | Section           | Source                                                  | Always shown?      |
|---|-------------------|---------------------------------------------------------|--------------------|
| 1 | Persona           | `prompts/persona/*.md` (concatenated)                   | Yes                |
| 2 | Current Context   | integration / channel / thread / user tags              | Yes                |
| 3 | Shared Memory Index | `data/memory/shared/INDEX.md`                         | If present         |
| 4 | User Memory Index | `data/memory/users/<int>/<user>/INDEX.md`               | If present         |
| 5 | User Profile      | `data/memory/users/<int>/<user>/user.md`                | If present         |
| 6 | Preferences       | `data/memory/users/<int>/<user>/preferences.md`         | If present         |
| 7 | Session Summary   | `data/sessions/.../current.md`                          | If present         |
| 8 | Recent Turns      | in-memory ring buffer (user/assistant/tool)             | Yes (if any)       |
| 9 | Current Input     | the incoming Envelope                                   | Yes                |

The user-scoped sections derive their path from `scope.FromEnvelope(env)`
(see [DECISIONS.md](DECISIONS.md) D-013). If the envelope has no user
attached (scheduler tick), only the shared INDEX appears.

The model accesses **everything else** — deeper memory, specific fact
files, session archives — through tools. It searches with `memory.search`,
reads with `memory.read`. This is the critical inversion vs. vector-search
systems: we hand the model an index + a grep tool instead of pre-injecting
similarity hits.

Memory content is always framed inside `<memory path="...">...</memory>`
fences with a system-level reminder that memory is data, not instructions.
See [MEMORY.md](MEMORY.md) on safety.

**Prefix-cache contract.** The section order above is deliberate. Stable
content (persona, shared memory, user memory, summary) sits at the front
of the system message; the recent-ring messages follow it; the new user
content is appended last. LM Studio and most OpenAI-compatible servers
reuse the KV prefix when the token sequence matches a prior call, so a
busy session pays prefill cost only on the new tail. Don't reorder these
sections without re-reading [DECISIONS.md](DECISIONS.md) D-017.

## Sessions

A session is scoped by `(integration, channel, thread)` — see
`Envelope.Key()` in [internal/integrations/integration.go](../internal/integrations/integration.go).

Three on-disk / in-memory tiers:

- **Short-term (in-memory)**: a ring buffer of the last N messages
  (`Session.recent`, cap = `maxTurns * 2`). Includes user, assistant, and
  tool messages — everything the model saw during that turn.

- **Short-term (mirrored to disk)**: `recent.json` next to the summary,
  rewritten atomically on every `Session.Append`. Carries the exact
  `[]llm.Message` plus a `kind` hint (`channel` / `dm`). On
  `SessionStore.Get`, a missing in-memory entry loads `recent.json` first
  so a restart resumes the conversation with the same tool calls / tool
  results / user turns the model already saw. See
  [DECISIONS.md](DECISIONS.md) D-016.

- **Long-term (file)**: a rolling compressed summary in
  `data/sessions/<integration>/<channel>/current.md`. Rewritten after each
  turn by the summarizer (see below).

The mapping from session key to filesystem path lives in
`SessionStore.SummaryPath` / `recentPath` — both just replace `:` with `/`.

**Idle rotation.** Idleness is kind-aware: `SESSION_IDLE_TIMEOUT`
(default 4h) governs channels, `SESSION_IDLE_TIMEOUT_DM` (default 168h)
governs one-on-one sessions. The active timeout comes from
`Envelope.IsDirect` (Discord sets it from `m.GuildID == ""`); the kind is
persisted in `recent.json` so a post-restart sweep applies the right
timeout to disk-discovered state. When a session is idle past its
threshold, `SessionStore.Get` (lazy) and the janitor's periodic sweep
both rotate it: `current.md` moves to `archive/<UTC-timestamp>.md`,
`recent.json` is removed, and the in-memory entry is dropped. Archive
files live until the janitor prunes them at `SESSION_TTL`. See
[DECISIONS.md](DECISIONS.md) D-011 and D-016.

## Summarizer

[internal/agent/summarizer.go](../internal/agent/summarizer.go) runs a
separate LLM call after each turn:

- Uses [prompts/summarizer.md](../prompts/summarizer.md) as its system prompt.
- Sent the previous summary + a flattened transcript of the ring buffer.
- Expected to return only the updated summary text.
- Written back to `current.md`.

The summarizer extracts *facts, decisions, unresolved threads* — not prose
recap. If it fails, the reply has already been delivered; we log and move on.

## Scheduler

Two flavours of scheduled work share the same bus-injection trick:

- **Static ticks** ([internal/scheduler/tick.go](../internal/scheduler/tick.go))
  publish a fixed Envelope on a fixed interval. Day one nothing is
  registered. Hook is in place for reflection passes, idle checks,
  proactive pings.

- **Dynamic jobs** ([internal/scheduler/jobs.go](../internal/scheduler/jobs.go))
  are model-authored timers. The `schedule.create` / `schedule.cancel` /
  `schedule.list` tools talk to a `JobManager` that owns a single robfig
  cron and a set of `time.AfterFunc` timers for one-shots. Each job is
  persisted as one JSON file under `data/scheduler/jobs/` so timers
  survive restart (one-shots whose `at` already passed are dropped on
  boot — misfire policy: skip; see [DECISIONS.md](DECISIONS.md) D-015).
  When a job fires, the synthetic Envelope inherits Integration / Channel
  / Thread / User from the originating turn, so the reply lands in the
  channel that asked for the reminder and memory routing still works.

The agent loop cannot tell either kind of scheduled fire from a Discord
message — which is the point.
