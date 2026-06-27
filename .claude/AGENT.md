# Agent

The agent loop lives in [internal/agent/](../internal/agent/). One worker
goroutine consumes Envelopes from the bus and drives each through a
ReAct-style tool-use loop until the model stops calling tools or the step
budget runs out.

## Loop shape

Pattern: **ReAct with native tool-use.** The loop is a small state machine
driven by the LLM's `tool_calls` response, not by parsed JSON text in the
message body. This is a hard break from the earlier homegrown
`{"response":..., "tool_calls":[...]}` protocol — see
[DECISIONS.md](DECISIONS.md).

On each step the loop:

1. Calls `llm.Client.Call(messages, tools)` with the full message list and
   the currently-registered tool specs.
2. Appends the assistant message (text + any tool_calls) to both the
   in-flight `messages` slice and the session transcript.
3. If the assistant produced final text, remembers it as `finalText`.
4. If the response contains tool_calls, dispatches each via the registry
   and appends the tool result as a `role: tool` message.
5. Breaks when there are no tool_calls, or when the step budget is hit.

At the end, the loop:

1. Delivers `finalText` via `Replies.Send(integration, channel, …)`.
2. Runs the summarizer against the session transcript (best-effort).

See [internal/agent/loop.go](../internal/agent/loop.go) for the real code.

## Key choices

- **Serial worker.** A single goroutine drains the bus. This is deliberate:
  it makes memory-file writes race-free without locks, keeps replies
  deterministically ordered, and avoids "two replies in parallel" footguns.
  A message from channel A blocks a message from channel B for the duration
  of one turn. Acceptable for a personal agent. Revisit if scale demands.

- **Native tool-use, not JSON-in-string.** The LLM returns structured
  `tool_calls`; we never parse the text body for a JSON envelope. This
  ties us to providers that support function calling. LM Studio does, when
  the loaded model does. This is a load-bearing decision — do not
  re-introduce JSON-in-string as a fallback.

- **Step budget** (`Config.MaxSteps`, default 8) and **wall-clock budget**
  (`Config.TurnBudget`, default 2 min) prevent runaway loops.

- **Interruptible.** The turn takes a context derived from the service
  context; cancellation during shutdown stops the turn mid-step.

- **Summarizer is best-effort.** Its failure must never block a reply.

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
