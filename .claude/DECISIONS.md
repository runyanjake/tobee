# Decisions

Lightweight ADR log. Each entry states **what was decided, why, and what
it costs** — so future-us can tell the difference between a load-bearing
choice and a coin flip.

When a decision changes, add a new entry. Don't silently rewrite the old one.

---

## D-001 — Native tool-use over JSON-in-string

**Status:** Accepted · **Date:** 2026-04 · Supersedes: earlier homegrown
`{"response":..., "tool_calls":[]}` protocol.

**Decision.** The LLM is called with the OpenAI `tools` / `tool_calls`
protocol. Assistant responses are parsed as structured `tool_calls`, never
as JSON embedded in the text body.

**Why.** The homegrown protocol needed a retry loop for models that
wrapped JSON in markdown fences, leaked `<think>` blocks, or just drifted.
Native tool-use pushes that burden onto the provider.

**Cost.** Ties us to providers that support function calling. LM Studio
does, when the loaded model does. A model that can't do tool-use cannot be
used with tobee as-shipped.

**Don't revert** without replacing with something more structured — the
JSON-in-string parser with retry is a local optimum we climbed out of.

---

## D-002 — `internal/` package prefix

**Status:** Accepted · **Date:** 2026-04

**Decision.** All project packages live under `internal/`. Top-level Go
files are limited to `main.go`.

**Why.** Idiomatic Go; prevents accidental imports from outside the module
if we ever publish a library; keeps the module self-contained.

**Cost.** None appreciable for a single-module app.

---

## D-003 — Memory sandboxed to `data/memory/`

**Status:** Accepted · **Date:** 2026-04

**Decision.** The `memory.*` tools can only read/write files below
`data/memory/`. Attempts to escape via `..`, absolute paths, or Windows
volume prefixes are rejected at the FS layer.

**Why.** The agent writes what it judges useful, without human approval.
Sandboxing limits blast radius.

**Cost.** OS-level file tools (read arbitrary file, list project tree) are
explicitly not a thing today. When they become useful they'll live in a
separate pack with their own scoping.

**Invariant.** All FS access goes through `memory.FS.resolve()`. Don't
reach around it.

---

## D-004 — No vector search, no reflection cron, no MCP in phase 1

**Status:** Accepted · **Date:** 2026-04

**Decision.** Ship with grep-based memory search, no background
consolidation passes, no MCP client or server.

**Why.** Each has non-trivial cost to implement and maintain, and the
benefit depends on conditions we haven't hit yet:

- Vector search pays off at 100+ files or when users ask semantically
  fuzzy questions. Our corpus is smaller.
- Reflection passes need enough signal to merit the token spend. Day-one
  memory is too sparse.
- MCP pays off when consuming or exposing tools to other hosts. Today
  tobee is both client and host.

**Cost.** Each is a "when needed" backlog item. The registry, memory
layout, and scheduler hook are shaped so adding any of them is additive.

---

## D-005 — Serial agent worker

**Status:** Accepted · **Date:** 2026-04

**Decision.** A single goroutine consumes the event bus and runs one
envelope to completion before picking up the next.

**Why.** Makes memory-file writes race-free without locks. Keeps replies
deterministic. Avoids "two replies in parallel on the same channel" bugs.

**Cost.** A message from channel A blocks a message from channel B for
the duration of one turn (up to the `TurnBudget`). Acceptable for a
personal agent. Not acceptable at scale — if we ever go multi-user, this
gets revisited.

---

## D-006 — Single persona prompt, not multi-file

**Status:** Superseded by D-012 · **Date:** 2026-04 · Supersedes: prior
`context/PROMPT.md` + `SOUL.md` + `TOOLS.md` split.

**Decision.** `prompts/persona.md` is the system prompt. Tool guidance
lives in tool `Description` fields, not a separate prompt file.

**Why.** The old split made the prompt hard to reason about as one thing.
Tool guidance belonged next to the tools anyway — the model sees it in
every tool schema.

**Cost.** Larger single file. Worth it.

---

## D-007 — No Discord `!command` escape hatch

**Status:** Accepted · **Date:** 2026-04

**Decision.** We did not port the old `!memory.list` style direct-action
invocation from the previous implementation.

**Why.** It was useful as a debugging aid during bring-up but ossified
into a second control plane. Add it back as a proper dev surface (a
`/debug` slash command? a CLI?) if it's needed again, not as a prefix
hack.

**Cost.** Slightly harder to poke tools without a live LLM. The tool pack
is unit-testable directly in Go if that becomes painful.

---

## D-008 — `data/` is gitignored entirely

**Status:** Accepted · **Date:** 2026-04

**Decision.** All of `data/` is in `.gitignore`. No seed files, no
`.gitkeep` markers.

**Why.** Memory is private to each developer's install. Fresh clones
should start empty.

**Cost.** New clones have no INDEX / user profile / preferences. The code
handles missing files gracefully — absent sections are simply skipped in
the context builder.

---

## D-009 — Reply sender registry, not a `send.*` tool

**Status:** Accepted · **Date:** 2026-04 · Supersedes an earlier sketch
where integrations registered `send.discord` as a regular tool.

**Decision.** Final assistant text is delivered via `agent.Replies`, a
small integration-keyed lookup table of `ReplySender` functions. It's not
modelled as a tool the LLM calls.

**Why.** Replying is the default end-state of a turn, not a thing the
model chooses. Making it a tool invites confusion ("when should I call
`send.discord`?") and loses the guaranteed delivery semantic.

**Cost.** Sending to a *different* channel than the originator would need
a real tool. Not built; straightforward to add when needed.

---

## D-010 — In-process janitor for session TTL

**Status:** Accepted · **Date:** 2026-04-22

**Decision.** Session summaries under `data/sessions/` are pruned by an
in-process goroutine ([internal/scheduler/janitor.go](../internal/scheduler/janitor.go))
that wakes on a fixed interval (1h) and deletes files whose mtime is older
than `SESSION_TTL` (default 7 days), then removes any directories that
become empty. Long-term memory under `data/memory/` is never touched.

**Why.** A sidecar container was considered and rejected as overkill for
~50 lines of deterministic "rm old files" work. In-process:
- Shares the deployment surface, logs, and signal handling.
- Runs deterministically without the LLM — no envelope, no agent spend.
- Idempotent and crash-safe: the next startup sweeps what a downtime missed.

Separate from `scheduler.Scheduler`, which exists for envelope-producing
ticks the agent should see. Cleanup is janitorial, not agent-visible.

**Cost.** If tobee is down, no cleanup runs — but the next startup catches
up. No runtime lock between janitor and summarizer; at the 7-day cutoff a
race is near-impossible in practice, and if it happens the next summarizer
write recreates the file.

**Knob.** `SESSION_TTL` (Go duration syntax, e.g. `168h`). Non-positive
disables the janitor entirely.

---

## D-011 — Idle-based session rotation; rotate-then-prune

**Status:** Accepted · **Date:** 2026-05-12 · Amends D-010.

**Decision.** A session is rotated (not deleted) once it has been idle for
`SESSION_IDLE_TIMEOUT` (default 4h). Rotation moves
`data/sessions/<int>/<chan>/current.md` to
`data/sessions/<int>/<chan>/archive/<UTC-timestamp>.md` and drops the
in-memory `Session` entry. The next message on that channel therefore
starts from an empty `Recent()` buffer with no Session Summary section in
the system prompt.

The janitor ([internal/scheduler/janitor.go](../internal/scheduler/janitor.go))
runs both the rotation sweep (via `SessionStore.SweepIdle`) and the
archive cleanup. `SESSION_TTL` no longer governs `current.md` directly —
it now controls how long files under `archive/` are retained before the
janitor removes them.

**Why.** Sessions are bursty; a quiet channel shouldn't resurrect a stale
rolling summary on the next message a month later. Rotating preserves
forensic history without polluting the active prompt. Two knobs let the
"reset to fresh" window be short (hours) while the "destroy the evidence"
window stays long (days).

**Cost.** Slightly more disk usage than the original delete-on-stale
design — bounded by `SESSION_TTL`. One extra env var to document.

**Invariant.** `data/memory/` is still never touched. The archive tree
only ever holds rotated summaries that the agent does not read.

---

## D-012 — Personality as a folder of numbered fragments

**Status:** Accepted · **Date:** 2026-06-26 · Supersedes: D-006.

**Decision.** The system prompt is assembled at startup from every `*.md`
file under `prompts/persona/`, sorted lexicographically and joined
with blank lines. Day-one fragments:

- `00-identity.md` — who tobee is.
- `01-tone.md` — how it speaks. Matter-of-fact; no pleasantries.
- `02-behaviour.md` — what it does (actions, memory consultation).
- `03-output.md` — format constraints.
- `04-safety.md` — boundaries, memory-as-data framing.

`cmd/tobee/main.go::readPersona` does the glob + sort + read. Tool
guidance still lives in tool `Description` fields, per D-006's tool half.

**Why.** Two reasons D-006 didn't anticipate:

1. *Editability.* Tightening tone shouldn't require scanning a 30-line
   blob to find the right paragraph. Fragments are addressable: open
   `01-tone.md`, change tone.
2. *Tone hygiene.* When tone instructions sit next to identity and
   tool-use guidance, they get diluted. Isolating them in one file makes
   it visible when the model's behaviour drifts from what we asked for.

D-006's worry was "hard to reason about as one thing." That risk is real
but mitigated here: the fragments are short, the numeric prefix is the
load-order contract, and the assembled prompt is still one continuous
string at runtime — no per-fragment context injection or templating.

**Cost.** A second source of truth for "where does tobee's tone live?"
— the answer is `prompts/persona/`, full stop. Anyone editing the
persona must remember to drop new fragments in this folder rather than
hard-coding strings in Go. CONVENTIONS.md's "prompts live in files"
rule already covers this.

**Don't revert** without first identifying which file in the folder was
the actual problem. If a fragment is causing drift, edit it. If the
whole split is causing drift, log a new decision.

---

## D-013 — Multi-user memory layout

**Status:** Accepted · **Date:** 2026-06-26 · Supersedes the single-user
flat layout under `data/memory/`.

**Decision.** Memory is split into two top-level slices: `data/memory/shared/`
(cross-user knowledge) and `data/memory/users/<integration>/<userId>/`
(one tree per individual). The user key derives from the inbound `Envelope`
and is sanitized to filesystem-safe characters by `internal/scope`.

The active scope rides on the per-turn `context.Context`. The agent loop
calls `scope.With(ctx, scope.FromEnvelope(env))` once before invoking the
LLM; tool handlers and reporters read it back via `scope.From(ctx)`. The
`memory.FS` itself stays user-agnostic — only the tool layer knows about
scope routing.

Every `memory.*` tool accepts an optional `scope` argument:

- `user` — writes/reads under the active user's tree. Errors clearly if
  no user is attached (e.g. on a scheduler tick).
- `shared` — writes/reads under `shared/`.
- `both` — read-only scopes (`memory.search`, `memory.list`) walk both.

**Why.** tobee is moving from "one user, many integrations" to "many
users, many integrations". Per-user trees give clean isolation without
inventing a user table; shared/ holds cross-cutting facts that don't
belong to anyone.

**Cost.** A breaking change to anyone's existing local `data/memory/`.
`data/` is gitignored (D-008) so this only bites individual developers
on upgrade — files must be moved manually into the new layout.

**Invariant.** `memory.search` and `memory.list` must never leak files
from one user's tree into another's. Enforced at the FS layer via
`SearchUnder(query, limit, relDir)` and the existing `List(relDir)`.
Never call them with the root dir from a user-scoped path.

---

## D-014 — Reporter registry for cross-subsystem introspection

**Status:** Accepted · **Date:** 2026-06-26

**Decision.** Subsystems that hold state worth surfacing (scheduler,
janitor, integrations) implement an `abilities.Reporter` and register on
a single `abilities.Registry`. The model-callable `status.report` tool
snapshots the registry and returns one composed JSON object keyed by
reporter name; each value has optional `doing` / `done` / `waiting`
buckets.

Reporters do their own relevance filtering — they know their staleness
rules better than the ability layer does. The status tool stays a dumb
composer.

**Why.** A "what are you up to?" question naturally cuts across
subsystems. Without a contract, the status tool would have to reach into
each subsystem directly, growing more coupling with every new feature.
The Reporter pattern is the smallest abstraction that lets new
subsystems (and future abilities) plug in additively.

CONVENTIONS.md normally warns against abstracting before the second
implementation. We took the hit here because we're explicitly designing
for N reporters from day one (scheduler + janitor + discord, with more to
come). The alternative — direct reach-in — couples worse and scales
worse.

**Cost.** A new abstraction surface. Small in-memory ring buffers in
scheduler and janitor (≤32 entries each) exist purely for reportability —
that state lives in-process and does not survive restarts.

**Don't revert** without first asking what specifically broke. If a
reporter is noisy, fix the reporter; if the contract is wrong, log a new
decision.

---

## D-015 — Dynamic scheduled jobs persisted as one JSON file per job

**Status:** Accepted · **Date:** 2026-06-26

**Decision.** The model schedules its own future prompts via a
`schedule.*` tool pack backed by `internal/scheduler.JobManager`. Jobs are
persisted as one JSON file per job under `data/scheduler/jobs/<id>.json`
(atomic tmp + rename writes; delete on cancel or one-shot fire). At startup
the manager replays every file, drops one-shots whose `at` is in the past,
and registers the rest.

Recurring jobs go through a single `github.com/robfig/cron/v3` instance
(one internal goroutine, min-heap of next-fire times). One-shot jobs use
`time.AfterFunc` directly. Cancellation goes through a per-job handle in a
shared map. When a job fires, the manager publishes a synthetic
`Envelope` onto the same bus integrations use — the agent loop cannot
distinguish it from a real inbound message, which is the point.

The fired envelope inherits `Integration` / `Channel` / `Thread` / `User`
/ `UserName` from the originating turn (carried by `scope.UserScope`,
extended for this feature). The reply therefore lands in the channel that
asked for the timer, memory tools route under the right user tree, and
the session transcript stays coherent. `Content` is prefixed with
`[scheduled fire: <name>]` so the model can tell self-fires apart.

**Why.** Reminders, follow-ups, and "check on this in an hour" are
high-value behaviours that don't fit into a single turn. The existing
static-tick `Scheduler` covers reflection / heartbeat hooks but not
arbitrary model-authored timers. Persistence is mandatory — a reminder
that vanishes on restart is worse than no reminder. One file per job
matches the rest of the project's "plain text, no DB" stance (D-008) and
keeps the cancel path a single `os.Remove`.

**Misfire policy: skip.** If tobee is down when a one-shot was supposed
to fire, the next boot deletes the file rather than running stale.
Recurring jobs simply pick up at their next natural fire. Running a
reminder hours late is worse than silence; the model can reschedule when
it learns the gap happened.

**Cost.**

- New dependency: `github.com/robfig/cron/v3`. Mature, small, well-scoped.
- New abstraction surface: `JobManager`, a `schedules` Reporter, and three
  tools. Justified by the same reasoning as D-014 — the alternative
  (model-callable shell-out, ad-hoc goroutine spawning) is worse.
- `scope.UserScope` now carries `Channel` / `Thread` / `UserName` in
  addition to user identity. `Key()` / `Dir()` are unchanged and still
  user-only, so the memory-FS sandbox invariant (D-003) is preserved.
- One-shot jobs lose precise sub-second accuracy after a process restart
  if the persisted `At` is in the future — we re-arm with `time.Until`
  which is fine for human timescales.

**Don't revert** without proposing an alternative for the persistence and
the routing-back-to-channel requirement; both are load-bearing.

---

## D-016 — Session state persists across restart; idle timeout is kind-aware

**Status:** Accepted · **Date:** 2026-06-27 · Amends D-011.

**Decision.** Two changes to how session state survives.

1. The short-term ring buffer is mirrored to disk on every `Session.Append`.
   The file is `data/sessions/<int>/<chan>/recent.json` and contains the
   exact `[]llm.Message` plus a `kind` hint. Writes are atomic (tmp +
   rename). On `SessionStore.Get`, a missing in-memory entry loads
   `recent.json` before serving the session, so a restart resumes the
   conversation mid-stream with the same tool calls / tool results / user
   turns the model already saw.

2. `SessionStore` now carries **two** idle timeouts:
   `SESSION_IDLE_TIMEOUT` (default 4h) for channels and
   `SESSION_IDLE_TIMEOUT_DM` (default 168h) for one-on-one sessions. The
   active timeout is chosen from `Envelope.IsDirect`, which integrations
   set on the inbound envelope (Discord uses `m.GuildID == ""`). The kind
   is persisted in `recent.json` so a post-restart sweep applies the
   right timeout to disk-discovered state.

**Why.** D-011's "start fresh after 4h idle" assumed all sessions look the
same. They don't: a busy channel goes quiet for an afternoon and the next
message should reasonably start clean; a DM goes quiet for two days and
the next message is almost always the continuation of a thread. Worse,
*all* sessions lost their ring buffer on every restart — only the lossy
rolling summary survived. The combined effect was that "context isn't
lost" was only true when the bot had been running continuously.

**Cost.**
- One JSON write per LLM message (capped to ~20 entries), atomic. At
  personal-bot scale this is noise.
- Disk format becomes load-bearing — a future change to `llm.Message`
  must remain JSON-round-trippable or the file silently dies (warned, not
  errored — we degrade to "no prior state"). Acceptable.
- `Envelope` grows a field (`IsDirect`). Cheap, backwards-default-safe
  (zero value = "treat as channel" = old behaviour). Integrations that
  don't set it get the old 4h timeout, which is correct for non-DM
  transports.
- Rotation now also removes `recent.json` (not archived — exact
  transcript has no forensic value once the summary is archived).

**Invariant.** `data/memory/` is still never touched. The new file lives
only under `data/sessions/`.

**Don't revert** without proposing how to keep mid-conversation context
across a restart. The "rolling summary only" baseline was measurably
worse.

---

## D-017 — System-prompt layout is prefix-cache-friendly by design

**Status:** Accepted · **Date:** 2026-06-27

**Decision.** The order in which `ContextBuilder.renderSystem` composes the
system prompt — persona → `<context>` tag → shared INDEX → user INDEX →
user profile → preferences → session summary — is treated as a contract.
The prefix that varies least across consecutive turns sits at the front.
The recent-ring messages follow the system message; the new user content
is appended last. This is the cache-friendly shape: LM Studio (and other
OpenAI-compatible servers) reuse the KV prefix when the token sequence
matches a prior call, and our serial worker means consecutive turns on
the same session share that prefix as long as memory hasn't changed.

**Why.** This was implicit, then load-bearing once we made sessions
persist (D-016). A longer ring buffer means a longer prompt; if the
prefix isn't cached, prefill cost grows turn-over-turn. Documenting the
ordering as a contract stops a future refactor from helpfully "tidying
up" the section order and silently destroying cache reuse.

**Cost.** Slight rigidity: anyone reorganising `renderSystem` must
preserve the front-loaded stable sections. Tradeoff worth it.

**Limit.** In a multi-user channel the per-user memory sections
(`users/.../INDEX.md`, `user.md`, `preferences.md`) change between turns
from different users — the prefix breaks at the shared-INDEX → user-INDEX
boundary. Mitigation would require lazy memory loading via tools instead
of always-inject. Not built; would be a real design change, log a new
decision if pursuing.

**Don't revert** without first checking whether prefill latency on long
DM sessions has actually regressed. The ordering's only job is cache
reuse; if a measurement disagrees, the measurement wins.

---

## D-018 — Repo layout aligned with Go + AI-agent conventions

**Status:** Accepted · **Date:** 2026-06-27 · Amends D-002.

**Decision.** Three layout changes were applied together so the repo
matches the conventions an outside reader (Go or AI) would expect:

1. The binary entry point moves to `cmd/tobee/main.go`. No Go files at
   the repo root. D-002's spirit is preserved — packages still live under
   `internal/`; the rule is now "Go files live under `cmd/<binary>/` or
   `internal/<pkg>/`."
2. The module path becomes `github.com/runyanjake/tobee` (canonical
   import URL), not the bare `tobee`. Imports across the tree updated to
   match.
3. The persona folder is `prompts/persona/`, not `prompts/personality/`.
   "Persona" is the term the AI community settled on (Anthropic Skills
   docs, system-prompt literature). The load contract is unchanged
   (numeric prefixes, lexicographic sort, concat with blank lines).

**Why.** All three were idiosyncratic relative to the de-facto standard.
The cost of staying off-convention grows: every new contributor (human
or agent) has to learn the local naming before they can navigate. Going
along with the convention is free and removes a class of surprise.

**Cost.** A one-time mechanical churn across imports and docs. No
runtime behaviour change.

**Don't revert** without a specific reason. The conventions are not
load-bearing in themselves; the alignment with the wider ecosystem is.

---

## D-019 — Workspace areas: configured host-file roots with sandboxed access

**Status:** Accepted · **Date:** 2026-06-27 · Amends D-003 (which deferred
OS-level file tools to a future "separate pack with their own scoping").

**Decision.** The agent gets a `workspace.*` tool pack
([internal/tools/workspace/](../internal/tools/workspace/)) backed by N
operator-configured "areas." Each area is a directory the model can list,
read, search, and (unless flagged read-only) write under. Areas are
defined entirely in `.env`:

    WORKSPACE_AREA_<NAME>          = /abs/path
    WORKSPACE_AREA_<NAME>_DESC     = human-readable purpose  (optional)
    WORKSPACE_AREA_<NAME>_READONLY = true                    (optional)

The `<NAME>` suffix is lowercased to form the area's identifier (what the
model passes as `area`). Each area is a `sandboxfs.FS` rooted at the
configured path — escape attempts (`..`, absolute paths, volume
prefixes) are rejected by the same `resolve()` guard that protects
`data/memory/`. A separate env var, `WORKSPACE_MAX_FILE_SIZE` (default
256 KiB), caps per-file I/O.

The pack ships five tools: `workspace.areas` (discovery),
`workspace.list`, `workspace.read`, `workspace.write`, and
`workspace.search` (defaults to `area="all"`, walking every configured
area in one call). The areas list is always injected into the system
prompt right after the persona — it is the most stable section
(config-time only) and so sits at the front per D-017's prefix-cache
contract. The discovery tool exists too so the model can re-fetch the
list explicitly, but in steady state it never needs to.

We also took the opportunity to extract `internal/memory/fs.go` into
`internal/sandboxfs/` (type stays `FS`, `MaxFileSize` becomes a
per-instance field passed at construction). Both memory and workspace
now share one well-tested sandbox implementation. This is the right time
for the extraction per CONVENTIONS.md — we have the second real caller.

**Why.** D-003 already anticipated this: it deferred OS-level file tools
on the explicit promise that, when needed, they would live in a separate
pack with their own scoping. The "scoping" word is doing the work — the
agent writes what it judges useful without human approval, so blast
radius must be bounded at the FS layer, not the prompt. Areas give the
operator one knob ("here are the directories I'm willing for tobee to
touch") and the sandbox guarantees the model can't reach past them. The
multi-area shape (rather than a single `WORKSPACE_ROOT`) reflects how
real workflows actually look: notes here, projects there, scratch
somewhere else, often with different write permissions.

Prompt-inject vs tool-call for discovery: prompt-inject won because the
data is tiny, stable across consecutive turns (cache-friendly), and the
alternative wastes a tool call every turn the model is unsure which
areas exist.

**Cost.**

- A new env-var surface (`WORKSPACE_AREA_*`, `WORKSPACE_MAX_FILE_SIZE`)
  documented in `.env.example`. Optional — zero areas = pack not
  registered = no behaviour change.
- One package extraction (`internal/memory` → `internal/sandboxfs`),
  mechanical, single signature change (`NewFS` now takes `maxFileSize`).
  `memory.MaxFileSize` constant is gone; callers pass 64 KiB explicitly.
- System prompt grows by a small `<workspace_areas>` block when at least
  one area is configured. Inserted at the cache-friendly front so it
  doesn't break prefix reuse.
- Discovery is fully visible to the model: every configured area's name
  and description sits in the persona slot. Don't name an area something
  you don't want the model to know exists.

**Explicitly not built.**

- `workspace.delete` / `workspace.move` — destructive ops with no
  human-in-the-loop. Log a new decision before adding either.
- `workspace.exec` — running arbitrary commands is a much bigger
  blast-radius jump and warrants its own ADR (D-020+) when proposed.
- Glob / regex in `list` — start with directory + plain-substring search.
- Per-user workspace trees mirroring memory's `users/<int>/<userId>/`
  layout (D-013). Could layer on later if a multi-user use case appears;
  not needed for the single-user case today.

**Don't revert** without proposing a replacement for: (a) the sandbox
guarantee, or (b) the discovery story. Both are load-bearing — the
former for safety, the latter for the model's ability to use the pack at
all without prompt acrobatics.

---

## D-020 — Think → plan → act with closed-loop replan as the loop shape

**Status:** Accepted · **Date:** 2026-06-28 · Amends D-001 (which set
ReAct + native tool-use as the loop body; this decision wraps the
ReAct sub-loop in an explicit plan/replan/synthesise scaffold).

**Decision.** A turn is no longer one ReAct sub-loop. It is four
phases coordinated by `processTurn` in
[internal/agent/loop.go](../internal/agent/loop.go):

1. **Plan** ([internal/agent/planner.go](../internal/agent/planner.go)).
   `Planner.Initial` runs one LLM call with the planner persona
   ([prompts/planner.md](../prompts/planner.md)) and a single virtual
   tool, `plan.commit`. The planner's system prompt also carries a
   read-only `<tools>` catalogue rendered from `tools.Registry` —
   the planner cannot call those tools itself but it sees their
   names + descriptions so it can plan steps that use them. Without
   the catalogue the planner has no signal that memory or workspace
   exist and falls back to the trivial-reply path on knowledge
   questions. Two outcomes: the model commits a structured plan
   (ordered `Step`s, each with a free-text `intent`), or it produces
   trivial-reply text and we skip the rest of the turn. The plan is
   JSON-decoded from the tool call's arguments; we never parse
   structure out of the text body (D-001 still holds).
2. **Act** ([internal/agent/executor.go](../internal/agent/executor.go)).
   `Executor.RunStep` runs the ReAct sub-loop, scoped to one Step at a
   time. Per-step iteration cap (`PLAN_MAX_STEPS_PER_STEP`, default 4)
   and turn-wide total cap (`PLAN_MAX_STEPS_TOTAL`, default 12) bound
   it. A step ends `done` when the model produces a final text
   response with no tool calls; it ends `failed` when either cap is
   hit without a terminal response.
3. **Replan** ([internal/agent/planner.go](../internal/agent/planner.go)).
   On step failure, `Planner.Revise` runs another LLM call with the
   prior plan + a `<replan>` system reminder and a `plan.revise`
   virtual tool. Hard cap `PLAN_MAX_REPLANS` (default 3); on
   exhaustion the loop finalises the plan with whatever steps did
   complete.
4. **Synthesise** ([internal/agent/synthesizer.go](../internal/agent/synthesizer.go)).
   Multi-step plans get one final LLM call using
   [prompts/synthesizer.md](../prompts/synthesizer.md) to compose the
   user-facing reply from the plan's step results. Single-step plans
   skip this and use the step's own result text as the reply, since a
   synthesis call would just paraphrase one input.

The plan is **turn-scoped**: it lives only inside `processTurn`. It is
not persisted to `recent.json` and does not survive a crash. The user
message is committed to the session transcript before the planner
runs, so a mid-turn crash leaves a recoverable anchor (the next turn
will see the orphaned user message and can react).

**The virtual tools.** `plan.commit` and `plan.revise` are advertised
only on the planner's LLM calls — they are never registered on the
global `tools.Registry`. `Planner.Initial` / `Planner.Revise` watch
for them in the response's `ToolCalls` and decode the arguments into a
`*Plan`. This is how we get structure without violating D-001's
"no JSON-in-string" rule: the structure rides on the native tool-call
protocol.

**Prefix-cache contract** (D-017) still holds. `ContextBuilder.ComposeSystem`
puts the plan render *last* among the data sections, so the prefix is
stable across executor sub-iterations within a step — only the trailing
`<plan>` block and the `<current_step>` reminder change. Persona swaps
between phases break the prefix once per phase boundary; that's
unavoidable and acceptable.

**Why.** The implicit ReAct loop conflated "decide what to do" with
"do it." Two observable failures fell out:

- Long multi-step turns where the model improvised step ordering
  badly (re-reading the same memory file twice, skipping the obvious
  first step). An explicit planner forces an ordering before any tool
  is called.
- Tool failures handled inconsistently. With no plan, "retry" and
  "give up" looked identical from the loop's perspective. With a
  plan, a failed step is a clear signal: revise or stop.

The plan-and-execute family (LangChain's `PlanAndExecute`, ReWOO, the
Anthropic computer-use loop) all converge on this shape. We picked
the closest variant — linear plan, single replan tool, conditional
synthesis — that fits tobee's "personal agent, fast simple turns
shouldn't pay structure tax" constraint.

**Trivial-turn fast path.** Critical for keeping latency reasonable.
Chitchat ("hi tobee") goes planner → reply, one LLM call, same cost
as the old loop. Multi-step turns pay `1 + N steps + (0 or 1)
synthesis` calls. Worst case (3 replans + total budget) is bounded by
the cap, not unbounded.

**Cost.**

- One mandatory extra LLM call per turn (the planner). Trivial turns
  amortise it; the planner doubles as the responder when no plan is
  needed.
- One synthesis LLM call per multi-step turn.
- The plan does not persist across crash. Acceptable per the user
  decision; mid-turn crashes are rare and the user message survives.
- `agent.Config` lost its `MaxSteps` field; budgets now split across
  `Executor.maxPerStep` / `totalBudget` and `Config.MaxReplans`. A
  one-time churn at the wiring site
  ([cmd/tobee/main.go](../cmd/tobee/main.go)).
- New env-var surface: `PLAN_MAX_STEPS_TOTAL`, `PLAN_MAX_STEPS_PER_STEP`,
  `PLAN_MAX_REPLANS`. Documented in `.env.example`.

**Explicitly not built.**

- **Reflection / post-turn self-critique.** Logged as a deliberate
  follow-up. The plan artifact gives us the substrate; a reflect
  pass becomes a fifth phase (Plan → Act → Replan? → Synth → Reflect)
  feeding memory. Not now.
- **DAG / parallel steps.** Linear plan. If parallelism becomes
  useful the `Step` type can grow `dependsOn` without breaking
  callers.
- **Plan persistence across restart.** Turn-scoped per user
  decision. Resuming a mid-flight plan is a separate ADR.
- **Skipping the planner for known-simple inputs.** Considered a
  triage heuristic; rejected for now because the planner *is* the
  simple-input responder via the trivial-reply path. Two-routers
  doesn't beat one.

**Don't revert** without first explaining how the replacement keeps
(a) a structured plan artifact the loop can inspect, and (b) the
single-LLM-call trivial-turn path. Both are load-bearing — the first
for the user-stated robustness requirement, the second for latency.

---

## Open questions

Not yet decided. Flag if you have an opinion.

- **Streaming replies.** Progressive Discord edits with a typing indicator
  vs. single-shot reply when done. Currently single-shot. Streaming would
  need chunk-aware edit logic in the Discord sender.
- **Provider abstraction.** Only target is LM Studio today; adding an
  Anthropic backend would mean splitting the LLM client behind an
  interface. Defer until there's a real second provider in flight.
- **INDEX.md curation policy.** Currently "append-only by default, humans
  do wholesale rewrites." Could let the agent own it. Risk: drift. Benefit:
  less manual upkeep. No change until there's signal.
