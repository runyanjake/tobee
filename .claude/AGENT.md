# Agent

The agent loop lives in [internal/agent/](../internal/agent/). One worker
goroutine consumes Envelopes from the bus and drives each through a
linear sequence: **plan → announce → execute → synth → deliver**. The
plan is a typed artifact; each step is a per-step ReAct sub-loop; the
synthesiser composes the final user-facing reply from the structured
plan + step results, not from the act loop's raw assistant messages.
Every LLM-authored artifact — the plan, each step's outcome, the final
reply — is committed via a required virtual tool call; free-form text
is a protocol violation that fails the turn (or step). Stored memory
is never pre-injected into the system prompt — the model fetches it
via `memory.*` tools. **Each envelope is a standalone turn: no session
ring buffer, no rolling summary, no cross-turn history** — anything
that must survive is written to and read from `memory.*` files. See
[DECISIONS.md](DECISIONS.md) D-001, D-024, D-025, D-026, and D-027.

## Per-turn state: `Turn`

`Turn` ([internal/agent/turn.go](../internal/agent/turn.go)) is the
single value threaded through every phase. It carries only what one
turn needs — no reference to persistent session state.

| Field          | Set by                                       |
|----------------|----------------------------------------------|
| Ctx            | processTurn (with turn timeout + scope)      |
| Env            | processTurn (from the envelope)              |
| Transcript     | ContextBuilder (just the current user msg); grown by Executor per step |
| Plan           | Planner.Run (nil if planner protocol fails)  |
| PlanMessageID  | processTurn (after announce, for edits)      |
| Reply          | Synthesizer.Finalize (via reply.commit)      |
| Reactions      | react (added on progress markers, cleared on success) |

## The phases

1. **Plan** ([internal/agent/planner.go](../internal/agent/planner.go)).
   LLM call with [prompts/planner.md](../prompts/planner.md) and the
   `plan.commit` virtual tool. `tool_choice=required`. The model
   commits an ordered `Plan` (goal + `Step`s, each with intent + tool
   scope). If the model emits text instead of `plan.commit`, the
   planner logs `PROTOCOL VIOLATION` at ERROR, appends a nudge to the
   transcript, and retries once. A second violation aborts the turn
   (no reply, ❌ reaction). `plan.commit` is advertised only on this
   call; never registered on `tools.Registry`. See D-025.

2. **Announce.** The plan is rendered with per-step emoji statuses
   (⏳/🔄/✅/❌) and sent via `Replies.Send`. The platform message ID
   is captured on `Turn.PlanMessageID`. A respond-only plan (no
   tool-bearing steps, e.g. a greeting) skips announcement — there's
   nothing useful to show.

3. **Execute** ([internal/agent/executor.go](../internal/agent/executor.go)).
   For each step in order: mark step running, edit the plan message
   via `Replies.Edit`, run `Executor.RunStep` (per-step ReAct sub-loop
   bounded by `PLAN_MAX_STEPS_PER_STEP`, default 4, and turn-total
   `PLAN_MAX_STEPS_TOTAL`, default 12), then edit the plan message
   again with done/failed status. Each iteration runs with
   `tool_choice=required` and advertises the planner-granted tools
   plus the virtual `step.finish({result})` tool. `step.finish` is
   the only legal termination for a tool-bearing step; free-form text
   is a protocol violation that costs one retry and then fails the
   step. A step with no declared Tools is respond-only (e.g.
   greeting): no LLM call, empty Result, marked done immediately —
   synth composes the reply from plan + persona. See D-025.

4. **Synthesise** ([internal/agent/synthesizer.go](../internal/agent/synthesizer.go)).
   LLM call with [prompts/synthesizer.md](../prompts/synthesizer.md)
   advertising only the `reply.commit({spoken, artifacts})` virtual
   tool. `tool_choice=required`. The Discord message is composed in
   Go by `renderReply` — `spoken` on top, each artifact as a
   triple-fenced block with an optional `lang` hint. Prose fences
   are never model-authored. Input is `persona + plan-as-typed-artifact
   + original user message`; the act loop's assistant messages are
   deliberately omitted (fix for the "self-talk" failure mode). One
   retry on protocol violation, then the turn delivers an empty reply
   (❌ reaction). See D-025.

5. **Deliver.** Send the reply via `Replies.Send`. Terminal reaction
   applied here and nowhere else — success clears the progress trail,
   failure adds ❌. No session persistence, no summariser, no
   post-turn work.

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

- **Strict tool-call protocol at every phase.** `plan.commit`,
  `step.finish`, and `reply.commit` are the only legal LLM outputs
  at the planner, executor, and synth phases respectively. Free-form
  text is a protocol violation: logged loudly, retried once with a
  nudge, then fails the turn (or step). No text-wrap fallback, no
  free-form terminal text, no model-authored fences. See D-025.

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

- **No sessions, no summariser.** Every envelope is a standalone turn.
  Cross-turn state lives in memory files, not in a ring buffer or a
  rolling summary. See D-027.

## Context building

[internal/agent/context.go](../internal/agent/context.go) composes the
system message and the sole outgoing user message per turn. Because
there is no cross-turn history, the shape is small and stable.

**System message** (sections, in fixed order):

| # | Section              | Source                                                  | Always shown?      |
|---|----------------------|---------------------------------------------------------|--------------------|
| 1 | Identity persona     | `prompts/persona/*.md` (concatenated)                   | Yes                |
| 2 | Phase instructions   | `planner.md` / `synthesizer.md` / "" for the executor   | Per phase          |
| 3 | Workspace areas      | boot-time `workspace.Areas` config                      | If configured      |
| 4 | Current context      | integration / channel / thread / user tags              | Yes                |
| 5 | Memory hint          | fixed `<memory>` block naming paths + `memory.*` tools  | Yes                |

**Transcript** (`ComposeTranscript`): a single `user`-role message
carrying `env.Content`. That's the entire outgoing conversation
alongside the system message — no ring buffer, no prior turns.

**Identity persona in every phase.** Previous versions passed the
phase prompt as if it were the persona, so the planner and synth
phases saw no `prompts/persona/*.md` content at all. `ComposeSystem`
now always emits `b.Persona` first, then the phase-specific block.

**Memory is not pre-injected.** As of D-025, stored knowledge —
`shared/INDEX.md`, the user's `INDEX.md`, `user.md`, `preferences.md`,
and everything under them — is reached exclusively via the `memory.*`
tools. Section 5 is a fixed reminder that names the paths and tools;
the file bodies live in the sandbox, not in the system prompt. Under
D-027 this doubles as the *only* mechanism for carrying anything
between turns: if the model wants a preference or fact to survive to
the next message, it must call `memory.write` / `memory.append`.

The user-scoped memory paths derive from `scope.FromEnvelope(env)`
(see [DECISIONS.md](DECISIONS.md) D-013). When the envelope has no
user attached (scheduler tick), the memory hint block calls that out
and only `scope="shared"` is usable.

This is the critical inversion vs. vector-search systems: we hand the
model a small handbook of memory tools and let it decide what to
fetch, instead of pre-injecting similarity hits or full index dumps.
See [MEMORY.md](MEMORY.md) on safety around treating memory content
as data rather than instructions.

**Prefix-cache contract.** The system-message content is now byte-stable
across turns for a given `(integration, user)` pair — no per-turn
summary, no growing ring buffer, only fixed instructions plus a small
`<context>` tag. LM Studio's KV cache reuses the whole prefix; every
turn pays prefill cost only for the new user message. Don't reorder
these sections without re-reading D-017.

## Startup prompt loading

`cmd/tobee/main.go` reads `PROMPTS_DIR/persona/*.md`, `planner.md`, and
`synthesizer.md` at boot. `logPromptsLoaded` emits an INFO line with
the byte counts and an ERROR (`prompts: MISSING`) when any of them
comes back empty — a container that ships without its prompt mount is
the running binary's most likely first-day failure mode, and the loud
log is what makes it obvious.

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
