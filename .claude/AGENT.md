# Agent

The agent loop lives in [internal/agent/](../internal/agent/). One worker
goroutine consumes Envelopes from the bus and drives each through a
linear sequence: **plan → announce → execute → synth → deliver**. The
plan is a typed artifact; each step is a per-step ReAct sub-loop; the
synthesiser composes the final user-facing reply from the structured
plan + step results, not from the act loop's raw assistant messages.
See [DECISIONS.md](DECISIONS.md) D-001 and D-024.

## Per-turn state: `Turn`

`Turn` ([internal/agent/turn.go](../internal/agent/turn.go)) is the
single value threaded through every phase:

| Field          | Set by                                       |
|----------------|----------------------------------------------|
| Ctx            | processTurn (with turn timeout + scope)      |
| Env            | processTurn (from the envelope)              |
| Session        | processTurn (loaded from SessionStore)       |
| Transcript     | ContextBuilder; grown by Executor per step   |
| Plan           | Planner.Run (with text-wrap fallback)        |
| PlanMessageID  | processTurn (after announce, for edits)      |
| Reply          | Synthesizer.Finalize                         |

## The phases

1. **Plan** ([internal/agent/planner.go](../internal/agent/planner.go)).
   One LLM call with [prompts/planner.md](../prompts/planner.md) and
   the `plan.commit` virtual tool. `tool_choice=required`. The model
   commits an ordered `Plan` (goal + `Step`s, each with intent + tool
   scope). If the model emits text instead of `plan.commit` (model
   non-compliance with the tool protocol), the text is wrapped as a
   one-step Plan whose Result is the text — letting the rest of the
   loop run uniformly on the trivial-input path. `plan.commit` is
   advertised only on this call; never registered on `tools.Registry`.

2. **Announce.** The plan is rendered with per-step emoji statuses
   (⏳/🔄/✅/❌) and sent via `Replies.Send`. The platform message ID
   is captured on `Turn.PlanMessageID`. The wrap-fallback case (one
   no-tools step) skips announcement — there's nothing useful to show.

3. **Execute** ([internal/agent/executor.go](../internal/agent/executor.go)).
   For each step in order: mark step running, edit the plan message
   via `Replies.Edit`, run `Executor.RunStep` (per-step ReAct sub-loop
   bounded by `PLAN_MAX_STEPS_PER_STEP`, default 4, and turn-total
   `PLAN_MAX_STEPS_TOTAL`, default 12), then edit the plan message
   again with done/failed status. No-tools steps (wrap fallback) are
   no-ops with Result pre-set.

4. **Synthesise** ([internal/agent/synthesizer.go](../internal/agent/synthesizer.go)).
   One LLM call with [prompts/synthesizer.md](../prompts/synthesizer.md)
   advertising no tools. Input is `persona + plan-as-typed-artifact +
   original user message` — the act loop's assistant messages are
   deliberately omitted. This is the fix for the "self-talk" failure
   mode: with no conversational tail to continue, the synth produces
   a one-shot reply rendering of finished work.

5. **Deliver.** Send the reply via `Replies.Send`, mirror into session,
   run the (best-effort) summariser.

## Plan-message editing

`Replies` carries an optional `MessageEditor` per integration alongside
the always-required `ReplySender`. Discord registers both. The agent
edits the plan-announcement message in place as step statuses change.
If the editor is missing or fails, status updates degrade silently
(debug log only) — the user still sees the final reply.

## Key choices

- **Serial worker.** A single goroutine drains the bus. Makes
  memory-file writes race-free without locks, keeps replies
  deterministically ordered, avoids parallel-reply footguns.
  Acceptable for a personal agent.

- **Native tool-use, not JSON-in-string.** The LLM returns structured
  `tool_calls`; we never parse the text body for a JSON envelope.
  Load-bearing.

- **Plan is a typed artifact.** Goal + ordered steps + per-step result.
  Used both for execution control (executor scopes tools per step) and
  for user comms (announcement + edits) and for synthesis (single
  structured input). The model's free-text output during the act loop
  is scratchpad — it never reaches the synthesiser.

- **Strict planner with one fallback.** `plan.commit` is the only
  legal output from the planner phase. When the model bypasses the
  tool protocol and emits text, the text is wrapped as a one-step
  plan whose Result is the text. This keeps the rest of the loop
  uniform on the trivial path without re-introducing a categorical
  branch. See D-024.

- **Synthesiser sees only structured input.** Persona + plan +
  original user message. No act-loop assistant messages. This
  eliminates the "self-talk" failure mode where the synth saw a
  conversational tail and continued it instead of rendering a final
  reply.

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
