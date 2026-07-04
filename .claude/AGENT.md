# Agent

The agent loop lives in [internal/agent/](../internal/agent/). One worker
goroutine consumes Envelopes from the bus and drives each through a
single continuous conversation: **plan → announce → execute → synth →
deliver**. Under D-029 this is **one chat per request** — a
`Conversation` object is created when the message arrives, gets a
single system prompt at position 0, and every phase appends to the
same `Messages` list. The planner, executor, and synth phases each
inject a user-role state template rendered from `prompts/state/*.md`
that carries the phase-specific instructions.

Every LLM-authored artifact — the plan, each step's outcome, the
final reply — is committed via a required virtual tool call
(`plan.commit`, `step.finish`, `reply.commit`); free-form text is a
protocol violation that fails the turn (or step). Stored memory is
never pre-injected into the system prompt — the model fetches it via
`memory.*` tools. Each envelope is a standalone turn: no session ring
buffer, no rolling summary, no cross-turn history — anything that
must survive is written to and read from `memory.*` files. See
[DECISIONS.md](DECISIONS.md) D-001, D-024, D-025, D-026, D-027,
D-028, and D-029.

## Per-turn state: `Turn` + `Conversation`

`Turn` ([internal/agent/turn.go](../internal/agent/turn.go)) is the
outer envelope-scoped state. It holds a pointer to `Conversation`
([internal/agent/conversation.go](../internal/agent/conversation.go)),
which owns the growing message list all phases share.

| Field                        | Set by                                       |
|------------------------------|----------------------------------------------|
| `Turn.Ctx`                   | processTurn (with turn timeout + scope)      |
| `Turn.Env`                   | processTurn (from the envelope)              |
| `Turn.Conversation`          | processTurn (seeded with the system message) |
| `Turn.PlanMessageID`         | processTurn (after announce, for edits)     |
| `Turn.Reply`                 | Synthesizer.Finalize (via reply.commit)      |
| `Turn.Reactions`             | react (added on progress, cleared on success)|
| `Conversation.Messages`      | seeded by NewConversation; appended by every phase |
| `Conversation.Plan`          | Planner.Run                                   |
| `Conversation.StepCursor`    | executor loop                                 |
| `Conversation.Finished`      | step.finish with `finished: true` sets this  |
| `Conversation.SurfacedKnowledge` | stub for future web/file search integration |

## The phases (one conversation)

1. **Plan** ([internal/agent/planner.go](../internal/agent/planner.go)).
   The rendered `prompts/state/plan.md` is appended as a user message
   to the fresh conversation, then the LLM call advertises the
   `plan.commit` virtual tool with `tool_choice=required`. The model
   commits an ordered `Plan` (goal + `Step`s with intent only —
   D-029 removed per-step `tools` and `memory_paths`). Protocol
   violations are retried once with a nudge; a second violation aborts
   the turn (❌ reaction). See D-025.

2. **Announce.** The plan is rendered with per-step emoji statuses
   (⏳/🔄/✅/❌/⏭️) and sent via `Replies.Send`. The platform message
   ID is captured on `Turn.PlanMessageID` and edited in place as
   steps run.

3. **Execute** ([internal/agent/executor.go](../internal/agent/executor.go)).
   For each step: append the rendered `prompts/state/execute_step.md`
   as a user message to the *same conversation*, then run the ReAct
   sub-loop against `Conversation.Messages`. Each iteration advertises
   the full `tools.Registry` plus the virtual `step.finish({result,
   finished})` tool with `tool_choice=required`. `step.finish` is the
   only legal termination; free-form text is a protocol violation
   (one retry, then failed). When `finished: true` is set on any
   `step.finish` call, `Conversation.Finished` flips true and the
   loop skips remaining steps (marked ⏭️). Bounds:
   `PLAN_MAX_STEPS_PER_STEP` (default 4), `PLAN_MAX_STEPS_TOTAL`
   (default 12). See D-025 / D-029.

4. **Synthesise** ([internal/agent/synthesizer.go](../internal/agent/synthesizer.go)).
   The rendered `prompts/state/synthesize.md` is appended as a user
   message, then the LLM call advertises only `reply.commit({spoken,
   artifacts})` with `tool_choice=required`. Because this runs on the
   same conversation, the synth *does* see the executor's assistant
   messages and tool results — the "self-talk" failure mode is
   mitigated by the strict `state/synthesize.md` framing ("do not
   continue the transcript, do not summarise, do not ask a
   follow-up") plus forced `reply.commit`. `renderReply` composes the
   Discord message in Go: `spoken` on top, each artifact as a
   triple-fenced block with an optional `lang` hint. One retry on
   protocol violation, then the turn delivers empty (❌).

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

- **Plan is a typed artifact.** Goal + ordered steps + per-step
  result. Used for execution control (executor iterates through
  steps), user comms (announcement + edits), and prompt data (state
  templates render `{{.Plan}}` and `{{.Step}}`). D-029 removed the
  per-step `tools` scoping — every step advertises every registered
  tool.

- **Strict tool-call protocol at every phase.** `plan.commit`,
  `step.finish`, and `reply.commit` are the only legal LLM outputs
  at the planner, executor, and synth phases respectively. Free-form
  text is a protocol violation: logged loudly, retried once with a
  nudge, then fails the turn (or step). No text-wrap fallback, no
  free-form terminal text, no model-authored fences. See D-025.

- **Synthesiser sees the full conversation.** Under D-029 the synth
  runs on the same growing `Conversation` as the executor. The
  "self-talk" failure mode (D-023) is instead mitigated by the strict
  `prompts/state/synthesize.md` framing plus forced `reply.commit`
  with `tool_choice=required`. If self-talk resurfaces in practice,
  the fallback is to build synth's messages as a slim `[system,
  original_user, step_results]` set — same shape as D-024. Not
  implemented today.

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

[internal/agent/context.go](../internal/agent/context.go) composes
**one** system message per request. It sits at `Conversation.Messages[0]`
and is not resent — the same conversation grows through every phase.

**System message** (built once):

| # | Section              | Source                                                  | Always shown?      |
|---|----------------------|---------------------------------------------------------|--------------------|
| 1 | System prompt        | `prompts/system/*.md` (concatenated: identity + tone + behaviour + output + safety + tools) | Yes |
| 2 | Workspace areas      | boot-time `workspace.Areas` config                      | If configured      |
| 3 | Current context      | integration / channel / thread / user tags              | Yes                |
| 4 | Memory hint          | fixed `<memory>` block naming paths + `memory.*` tools  | Yes                |

**Phase-transition messages** are user-role, rendered from
`prompts/state/*.md` at the moment they're appended:

| Template               | Rendered by     | Data available in template |
|------------------------|-----------------|-----------------------------|
| `state/plan.md`        | Planner.Run     | `.UserInput`                |
| `state/execute_step.md`| Executor.RunStep| `.Step`, `.StepNumber`, `.StepTotal`, `.Plan`, `.AvailableTools` |
| `state/synthesize.md`  | Synthesizer     | `.Plan`                     |

Templates use Go `text/template`. Register a `FuncMap` in
`internal/agent/state.go` if you need extra template functions
(`join` is provided; add more as needed).

**Tool catalogue is a static file.** Under D-028, the `<tools>`
catalogue lives in `prompts/system/05-tools.md`. There is no
`renderToolCatalogue` walking `tools.Registry` at request time. The
`tools=[…]` API parameter is still what enforces which tools each
LLM call can invoke.

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

`cmd/tobee/main.go` reads at boot:

- `PROMPTS_DIR/system/*.md` via `readSystemPrompt` — concatenated into
  the system-message blob.
- `PROMPTS_DIR/state/*.md` via `agent.LoadStateTemplates` — parsed as
  `text/template` and cached by base name.

`logPromptsLoaded` emits an INFO line with `system_chars` and the
loaded template names, and a loud ERROR (`prompts: MISSING`) when the
system prompt is empty or any of the required state templates
(`plan`, `execute_step`, `synthesize`) is missing. A container that
ships without prompts silently misbehaves otherwise; the loud log is
what makes it obvious.

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
