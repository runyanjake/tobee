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

## D-021 — Status tools render their own text; LLM relays verbatim

**Status:** Accepted · **Date:** 2026-06-28 · Amends D-014 (which had
`status.report` return composed JSON for the LLM to summarise).

**Decision.** The single `status.report` tool is replaced by two tools,
both of which return pre-rendered deterministic strings:

- `status.summary` — a brief few-sentence overview for general "what
  are you up to?" inquiries.
- `status.report` — a strict multi-line full-detail block per
  subsystem (`Doing` / `Done` / `Waiting`), used when the user asks
  for specifics.

The `abilities.Reporter` contract drops `Report(ctx, since)
(ReportData, error)` and gains
`Render(ctx, since) (full, summary string)`. Each reporter formats its
own state; the registry composes sections (`RenderReport`) or joins
summaries (`RenderSummary`). The `ReportData` struct and the
`json.RawMessage` Doing/Done/Waiting buckets are gone.

Tool descriptions explicitly instruct the model to relay the output
verbatim, and [prompts/planner.md](../prompts/planner.md) routes
status questions to the appropriate tool (with status no longer
qualifying for the trivial-reply path).

**Why.** D-014's JSON-then-summarise path produced sporadic phrasing:
the same underlying state turned into a different reply each turn,
sometimes dropping sections, sometimes inventing reassurances. The
variability lived in the LLM's translation step. Moving the format
into the reporter removes that step — the LLM still picks *which*
tool to call, but it no longer rewrites the answer.

The summary/report split keeps the model's discretion useful: it
decides "casual ask" vs. "detailed ask" and routes accordingly, but
the wording at each level is fixed.

**Cost.**

- Reporter implementations got longer — they now format text per
  bucket instead of marshalling a typed struct. Mechanical; one-time.
- Two tools instead of one. Trivially more surface in the tool
  catalogue; selection is rule-bound, not heuristic.
- The model loses the ability to reason over the structured JSON
  (e.g., "answer just the next-fire times"). If that comes up, we add
  a third tool or restore a JSON view alongside the rendered ones.

**Don't revert** without first checking whether the LLM is in fact
producing reliable summaries from JSON in the current model / temp
configuration. The phrasing-drift symptom is the load-bearing reason
to keep this contract.

---

## D-022 — Triage phase + state-machine driver; planner narrows to revise-only

**Status:** Accepted · **Date:** 2026-06-29 · Amends D-020 (which placed
the simple-input responder on the planner via a "trivial reply" path)
and D-021 (whose status-routing prose moves out of planner.md into the
triage contract).

**Decision.** Two coupled changes:

1. A new **triage** phase runs before the planner on every turn. One
   LLM call with the [prompts/triage.md](../prompts/triage.md) persona
   and three virtual tools: `triage.respond`, `triage.plan`,
   `triage.status`. The model picks exactly one — the category becomes
   the routing decision; the payload becomes the inputs for the next
   phase. `triage.plan` carries the same shape the old `plan.commit`
   did; `triage.respond` carries a verbatim reply; `triage.status`
   carries one of `status.summary` / `status.report`.

2. `processTurn` is rewritten as a **state-machine driver**. Per-turn
   state lives on a single `*Turn` value (env, session, transcript,
   triage result, plan, reply). Phases are `phaseFn` methods on
   `*Agent` that mutate `*Turn` and return the next phase to run or
   `nil` to terminate. The state graph:

       phaseTriage ─┬─► phaseRespond         ─► nil
                    ├─► phaseStatusDispatch  ─► nil
                    └─► phaseExec ─┬─► phaseReplan ─► phaseExec
                                   └─► phaseSynth  ─► nil

   `phaseStatusDispatch` calls the named status tool directly through
   `tools.Registry` — no executor LLM call. Per D-021 the output is
   already pre-rendered; passing it through an LLM only invites drift.

The `Planner` type narrows to **revise-only**. `Planner.Initial` and
`plan.commit` are removed; `plan.revise` stays. `prompts/planner.md`
is rewritten as a replan-after-failure persona (no trivial-reply
section, no status-routing section, no two-outputs framing).

**Why.** D-020's planner-as-responder design produced sporadic
hallucinations on knowledge questions: when the planner saw a
"do you remember…?" message it could (and increasingly did) take the
trivial-reply path and answer from its head rather than commit a
plan that hits memory. The forcing function we wanted — "every
non-chit-chat input must commit a plan" — was prose guidance, not a
schema constraint, and the model drifted off it.

The three-way categorical commit fixes that structurally: the model
cannot emit free text, only one of three tool calls, each with its
own schema and description. `triage.respond`'s description carries
a hard "ONLY for chit-chat — never for anything that may be in
memory" framing, and the asymmetry between false-positive cost
(hallucinated answer) and false-negative cost (one extra LLM call)
is spelled out in the persona itself. Defaulting to plan is cheaper.

The state-machine refactor is the other half. The previous
`processTurn` already passed `ctx`, `env`, `transcript`, `session`,
`plan`, `step`, `prev`, `reason` around as ad-hoc args; adding
triage and status-dispatch as phases would have fanned that out
further. Threading a single `*Turn` plus a phase-function pattern
keeps every state a first-class function with one signature,
which lets us add future states (reflection? clarification?)
without touching unrelated phases.

D-020 explicitly rejected "skipping the planner for known-simple
inputs" because the planner *was* the simple-input responder. That
reasoning no longer applies — we've split the responder role
(now on Triage) from the replan role (still on Planner). There is
no "second router"; there's one router with a stricter, structured
contract.

**Cost.**

- Simple chit-chat turn: 1 LLM call (triage, returns `triage.respond`).
  Same as today.
- Status turn: 1 LLM call (triage) + 1 deterministic tool dispatch.
  Strictly cheaper than today's planner-then-executor path (2 LLM calls).
- Complex turn: 1 LLM call (triage commits the plan) + N executor +
  optional synthesis. Same call count as today — the triage call IS
  the planner call; we did not add an extra LLM round-trip.
- Replan path: 1 extra LLM call per replan, as before.
- New surface: `prompts/triage.md`, `internal/agent/triage.go`,
  `internal/agent/turn.go`, `phaseFn` driver in `loop.go`.
- New wiring in `cmd/tobee/main.go`: `agent.New` now also takes
  `*tools.Registry` (for direct status dispatch) and `*Triage`.

**Extensibility seam.** `TriageResult.Metadata` is a forward-compatible
bag (`map[string]any`) for future enrichment — intent labels, topic
classifiers, user-state hints. The schema for each virtual tool can
also grow optional fields without breaking callers. When a real second
metadata axis appears, we either grow the existing tools or split
triage into two phases (classify-then-route); both are additive.

**Invariants preserved.**

- Native tool-use only (D-001) — the routing decision is carried by
  `tool_calls`, never parsed from text.
- Prefix-cache contract (D-017) holds within each phase. Persona swaps
  between triage / executor / synthesizer break the prefix once per
  phase boundary, which was already true under D-020.
- Status tools relay verbatim (D-021) — phaseStatusDispatch passes the
  tool output through without an LLM call.
- Serial worker (D-005) unchanged. One goroutine per turn.

**Explicitly not built.**

- **Categorisation beyond simple/plan/status.** The metadata seam is
  there; we didn't fill it. Add categories when a downstream phase
  needs to branch on them.
- **Triage prompt-cache reuse across turns.** Triage runs once per
  turn with a session-tail-only transcript; cache wins are bounded.
  Worth measuring before optimising.
- **Replacing the executor's per-step ReAct sub-loop with state
  machine phases.** The inner loop lives inside `Executor.RunStep`.
  Pulling it out would be a separate decision.

**Don't revert** without first explaining how the replacement keeps
(a) the three-way categorical routing (the forcing function for the
hallucination fix), (b) the direct status dispatch (cheaper than
LLM-paraphrased status), and (c) the per-turn `*Turn` value as the
single state seam new phases can grow off.

---

## D-023 — Unified ReAct loop + always-on synthesis

**Status:** Accepted · **Date:** 2026-06-29 · Supersedes D-020
(plan-and-execute scaffold) and D-022 (triage gate + state-machine
driver). Regresses D-021 (status verbatim relay) by explicit choice.

**Decision.** A turn is two phases, not five:

1. **Act loop.** One LLM call per iteration. The model sees the
   persona, the data sections, the recent transcript, and every
   registered tool. It either calls tools or produces terminal text
   (zero tool calls = loop ends). Terminal text on the very first
   iteration is the trivial path — common, expected, not an error.
   Bounded by `LOOP_MAX_ITERATIONS` (default 12).
2. **Synthesis.** Always runs. Reads the act-loop transcript (tool
   calls, tool results, terminal text) and composes the one outbound
   message the user sees. Tone, formatting, and length live here.
   The act loop is the model's scratchpad; the synthesiser is the
   user-facing voice.

`processTurn` becomes a linear sequence: build `Turn` → `Executor.Run`
→ `Synthesizer.Finalize` → `deliver`. No `phaseFn` driver, no
categorical routing, no `Plan` / `Step` types, no replan budget.
`Turn` keeps the context-object role from D-022 minus the Plan/Triage
fields.

**Why.** D-022's `tool_choice: "required"` invariant turned out not
to hold for the loaded LM Studio model: on simple greetings ("Hey
@TOBEE") the model emitted correct text ("Hello! How can I assist
you today?") with zero tool calls, and the triage phase rejected it
as "no recognised tool call." The forcing function bounced off the
model. We tried prompt tightening and tool_choice flags; the model
ignored both.

Rather than bolt on a text-as-respond fallback (and grow a thin edge
case per failure mode), we unify: a turn is "model thinks and may
act, then we synthesise a reply." Greetings, knowledge questions,
status questions, and action requests all flow through the same
shape. The model picks tools off a menu of concrete actions; the
synthesiser produces the reply. "No tool call needed" becomes the
expected trivial path, not a categorical mismatch.

Concrete actions remain concrete (the tool registry is the menu),
but the model owns the choice — no upfront category commit, no
forced tool call.

**Regressions explicitly accepted.**

- **D-021 status verbatim.** Status tools still pre-render
  deterministic text, but the synthesiser is now free to reword
  them. User-accepted formatting drift in exchange for one uniform
  shape. Revisit if the rewording materially loses information.
- **D-020 plan-and-execute structure.** No explicit ordering
  artifact. The model improvises step order inside the act loop.
  If empirical badness recurs (re-reading the same memory file,
  skipping the obvious first step), the fix is prompt-side or a
  reflection pass, not re-introducing a separate planner.

**Cost.**

- Trivial turn: 2 LLM calls (act loop with zero tool calls + always-
  on synthesis) vs. 1 under D-022. The synthesis tax buys consistent
  tone enforcement; the user explicitly accepted it.
- Non-trivial turn: N act-loop iterations + 1 synthesis. Same shape
  as D-022's exec + synth path for multi-step plans.
- Surface deleted: `internal/agent/triage.go`, `planner.go`,
  `plan.go`, `prompts/triage.md`, `prompts/planner.md`, the phaseFn
  driver. ~600 lines of agent-package code gone.
- Env-var surface: `PLAN_MAX_STEPS_TOTAL` / `PLAN_MAX_STEPS_PER_STEP`
  / `PLAN_MAX_REPLANS` replaced with `LOOP_MAX_ITERATIONS`.

**Invariants preserved.**

- Native tool-use only (D-001) — the act loop reads structured
  `tool_calls`; the synthesiser advertises no tools.
- Sandboxed FS (D-003), in-process janitor (D-010), session idle
  rotation + persistence (D-011 / D-016), per-user memory layout
  (D-013), reporter registry (D-014), dynamic scheduled jobs (D-015),
  prefix-cache contract (D-017), workspace areas (D-019) — all
  unchanged.
- Serial worker (D-005) — single goroutine still drains the bus.

**Explicitly not built.**

- **Reflection pass after synthesis.** Logged as a deliberate
  follow-up. The transcript is the substrate; a reflect call would
  become phase 3, feeding memory or self-critique.
- **Streaming the act loop's tool calls back to the user.** Today
  the user sees only the synthesised reply. A future "thinking…"
  indicator could surface in-progress activity but is a Discord-side
  change.
- **Forced tool choice tuning.** D-022's `tool_choice: "required"`
  wiring stays in the client; the act loop just doesn't use it. Any
  future phase that needs forced structure can opt in.

**Don't revert** without first explaining how the replacement
handles the trivial-input case ("Hey @TOBEE" → correct reply) and
how it avoids fanning out edge cases per failure mode. The whole
point of this decision is the one-shape-for-all-turns uniformity;
losing that loses the win.

---

## D-024 — Strict plan → announce → execute → synth; synth sees only structured input

**Status:** Accepted · **Date:** 2026-06-29 · Supersedes D-023.
Restores much of D-020/D-022's plan-execute structure with a strict
contract on synth's input and a single text-wrap fallback for model
non-compliance with the tool protocol.

**Decision.** Every turn is four phases (then deliver):

1. **Plan.** One LLM call with [prompts/planner.md](../prompts/planner.md)
   and the `plan.commit` virtual tool. `tool_choice=required`. The
   model commits an ordered `Plan` (goal + steps, each with intent +
   tool scope). The planner persona frames plain text as a "protocol
   violation" (per Cline / II-agent system-prompt conventions).
2. **Announce.** Plan rendered with emoji statuses (⏳/🔄/✅/❌) and
   sent to the user. The platform message ID is stored on `Turn`
   for in-place edits as step statuses change. The wrap-fallback
   case (see below) skips the announcement.
3. **Execute.** For each step in order: mark running, edit plan
   message, run `Executor.RunStep`, edit again with done/failed
   status. Per-step ReAct sub-loop scoped to the planner-granted
   tools. Bounds: `PLAN_MAX_STEPS_PER_STEP` (default 4),
   `PLAN_MAX_STEPS_TOTAL` (default 12).
4. **Synth.** One LLM call with [prompts/synthesizer.md](../prompts/synthesizer.md)
   advertising no tools. Input is `persona + plan-as-typed-artifact
   + original user message`. The act loop's assistant messages are
   deliberately omitted. Synth prompt borrows Cline's `attempt_completion`
   discipline verbatim ("the work is complete… do not plan, do not
   announce next steps, do not ask questions").

**Synth's input contract.** The single most load-bearing change vs
D-023. Under D-023, the synth saw the full transcript ending in the
act loop's last assistant message; for trivial inputs that message
was already a conversational reply, and synth produced a *continuation*
("Feel free to let me know!") instead of a *rendering*. With synth's
input restricted to plan + step results + original user message,
there is no conversational tail to continue. The plan is the
ground-truth record of what happened; synth's only job is to render
it as one user-facing message.

**Trivial-input handling — strict + single fallback.** The model
must commit `plan.commit` even for greetings (one-step plan with
empty tools list). If the model emits text instead, `Planner.Run`
wraps the text as `Plan{Steps:[Step{Status: done, Result: <text>}]}`
and the loop proceeds: no announcement (one no-tools step is
unworth announcing), execute is a no-op (Result pre-set), synth
renders the wrap-fallback text into a tobee-voiced reply. One
edge case lives in `Planner.Run`; nowhere else in the loop branches
on the wrap case.

**Plan-message editing.** `Replies` carries an optional `MessageEditor`
per integration alongside `ReplySender`. Discord registers both —
`ChannelMessageSend` returns the message ID; `ChannelMessageEdit`
mutates it. As step statuses change, the agent calls
`Replies.Edit` to update the announcement message in place. If the
editor is missing or the edit fails, status updates degrade silently
(debug log only) — the user still sees the final synth reply at
turn end.

**Why this returns to plan-execute shape vs D-023's collapsed loop.**
D-023 produced the "self-talk" failure mode on real traffic
("Hey @TOBEE" → act loop emits "Hello!" → synth continues with
"Feel free to let me know! 😃" instead of rewriting). The root
cause was structural: open-ended ReAct + always-synthesis with
synth seeing the transcript meant synth couldn't distinguish
"rewrite this" from "continue this." The plan-execute split with
synth restricted to structured input separates the two cleanly.

**Reference inputs.** The system-prompt design draws on prompts in
[LouisShark/chatgpt_system_prompt](https://github.com/LouisShark/chatgpt_system_prompt),
specifically: Cline's `attempt_completion` discipline for the
synthesiser; II-agent's "plain text is a protocol violation" for the
planner; Manus / Devin / Suna's "plan is a typed artifact, not prose"
framing for plan.commit's contract.

**Cost.**

- Trivial turn (greeting): 2 LLM calls (planner + synth). Same as
  D-023.
- Multi-step turn: 1 planner + N executor + 1 synth + 1+N user-side
  message edits. Edits are cheap (single Discord API call each).
- Surface added back: `internal/agent/plan.go`, `planner.go`,
  `prompts/planner.md`. `Replies` gained `MessageEditor` registration.
  Discord gained `editReply`. `agent.New` gained a `*Planner` arg.
- Env vars: `LOOP_MAX_ITERATIONS` replaced with the D-022-era pair
  `PLAN_MAX_STEPS_PER_STEP` (default 4) + `PLAN_MAX_STEPS_TOTAL`
  (default 12).
- New prompt-level debug logging: every LLM call now logs its
  prompt (system size + per-message roles + tail content) at debug
  via a shared `logPrompt` helper in `internal/agent/log.go`.

**Regressions explicitly accepted.**

- **D-021 status verbatim relay.** Status tools still pre-render
  deterministic text, but the synthesiser is free to reword them.
  User-accepted formatting drift in exchange for one uniform shape.
  Same trade made in D-023; unchanged here.

**Invariants preserved.**

- Native tool-use only (D-001).
- Sandboxed FS (D-003), in-process janitor (D-010), idle rotation
  + persistence (D-011 / D-016), per-user memory layout (D-013),
  reporter registry (D-014), dynamic scheduled jobs (D-015), prefix-
  cache contract (D-017), workspace areas (D-019) — all unchanged.
- Serial worker (D-005).

**Explicitly not built.**

- **Plan revise loop.** D-020's `plan.revise` flow on step failure
  is not restored. Step failures surface to the user via the
  synth's "failed: <reason>" framing. If empirical badness recurs
  (steps that obviously could be retried with a small tweak), the
  revise loop is a logical next addition — log a new decision then.
- **Reply-to-message fallback when edit is missing.** The user
  said reply-to-message would be acceptable when in-place edit is
  not. Not implemented today since Discord supports edit; will be
  needed when a non-edit integration is added.
- **Per-step user-visible progress as separate messages.** Edit-in-
  place is the chosen channel for progress. Falling back to
  per-step new messages is intentionally avoided as too chatty.

**Don't revert** without first explaining how the replacement (a)
prevents synth from continuing the conversation rather than rendering
it, (b) handles trivial inputs without per-failure-mode edge cases,
and (c) maintains the typed-plan artifact the executor and synthesiser
both depend on.

---

## D-025 — Strict tool-call protocol at every phase; no free-form fallbacks

**Status:** Accepted · **Date:** 2026-07-04 · Partially supersedes
D-024's "single text-wrap fallback" clause; the rest of D-024
(four-phase shape, synth's structured-input contract, plan-message
editing) is unchanged.

**Decision.** Every LLM-authored artifact in a turn is committed via
a required virtual tool call. Free-form text is a protocol violation
at all three phases:

1. **Planner.** `plan.commit` remains the only legal output. The
   text-wrap fallback in `Planner.Run` is removed. On a non-tool-call
   response the planner logs `PROTOCOL VIOLATION` at ERROR, appends
   a nudge to the transcript, and retries once. A second violation
   returns an error; the turn aborts before announce (no reply, ❌
   reaction, loud log).
2. **Executor.** A virtual `step.finish({result})` tool is advertised
   alongside the planner-granted tools on every executor iteration,
   with `tool_choice=required`. `step.finish` is the only way a
   tool-bearing step can terminate cleanly; `result` is the outcome
   string the synthesiser consumes. Free-form text (previously
   accepted as "terminal text ends the step") is a violation: log,
   nudge, retry once, then fail the step. A step with an empty
   `tools` list is still respond-only — no LLM call at all — but the
   Result is now empty (rather than pre-populated by the planner
   text-wrap path), and synth composes the reply from plan +
   persona alone.
3. **Synthesiser.** A virtual `reply.commit({spoken, artifacts})`
   tool is the only advertised tool, with `tool_choice=required`.
   The Discord message is composed in Go by `renderReply`: `spoken`
   on top, each `artifact` rendered as a triple-fenced block with an
   optional `lang` hint. The model no longer writes fences —
   formatting rules are code, not prose. One retry on violation,
   then the turn delivers an empty reply (❌ reaction).

**Why.** D-024 kept one free-text seam per phase (planner wrap,
executor terminal text, synth prose output) because it looked cheap
and preserved trivial-input uniformity. In practice the seams gave
the model latitude to choose the format — the planner sometimes
skipped `plan.commit` on greetings, the executor sometimes narrated
mid-step and short-circuited, the synth occasionally forgot the
persona's fence-vs-speech distinction and dumped code without
backticks. Every seam turned into a "the model decided" bug class.
Closing all three moves format ownership from prose-guided model
behaviour to code-enforced protocol, which is what the tool-calling
API is for.

**Trivial-input handling — restated.** Greetings still get a plan;
the plan still has one step with an empty `tools` list; the executor
still skips the LLM call for that step; synth still composes the
reply. What's gone is the "wrap raw text as a fake plan" recovery
path when the planner ignores `plan.commit`. Under D-025, that path
is a bug we log and abort on rather than one we paper over.

**What retry-once buys.** Local LLMs (LM Studio, small-to-medium
models) occasionally miss the tool-call framing on the first try
and get it right on the second when the transcript reminds them.
One nudge-and-retry per phase catches that without materially
expanding the budget: at worst a turn spends 6 LLM calls (3 phases
× 2 attempts) instead of 3, and only when the model is misbehaving.
Two retries would just cover for a broken model.

**Loud logging.** Every violation logs at ERROR with the greppable
prefix `PROTOCOL VIOLATION` and structured fields:
`phase, attempt, expected_tool, finish, text_chars, text_preview,
tool_calls_count, tool_calls`. Grep is the diagnostic.

**Cost.**

- Best case: unchanged — 2 LLM calls for a greeting (planner +
  synth), 1 + N + 1 for a working turn. Same as D-024 on the happy
  path.
- Worst case: doubled per phase where a retry fires. Bounded by the
  hard cap of one retry per phase.
- No new runtime dependencies. Two new virtual tools
  (`step.finish`, `reply.commit`) live only in the phase that
  advertises them; neither is registered on `tools.Registry`.
- Prompt surface changed: `prompts/planner.md`,
  `prompts/synthesizer.md`, and `prompts/persona/02-behaviour.md`
  now describe the strict contract. The user-facing rendering rules
  in `prompts/persona/03-output.md` are no longer load-bearing at
  synth time — `renderReply` enforces them — but stay as guidance
  for other contexts.
- Go-side surface changed: `renderReply` in
  `internal/agent/synthesizer.go` owns the fence rendering;
  `dispatchCalls` in `internal/agent/executor.go` owns the step
  termination branch.

**Invariants preserved.**

- Native tool-use only (D-001), sandboxed FS (D-003), serial worker
  (D-005), in-process janitor (D-010), idle rotation + persistence
  (D-011 / D-016), per-user memory layout (D-013), reporter registry
  (D-014), dynamic scheduled jobs (D-015), prefix-cache contract
  (D-017), workspace areas (D-019), synth's structured-input
  contract (D-024) — all unchanged.
- Four-phase plan → announce → execute → synth shape (D-024) —
  unchanged.

**Explicitly not built.**

- **N-way retries.** One retry per phase; no exponential backoff
  or retry-until-success loops. A model that can't hit the protocol
  after one nudge is broken and should be replaced.
- **Multiple `reply.commit` calls.** The schema allows multiple
  artifacts inside one call. A model that emits two `reply.commit`
  tool_calls is a violation; only the first is honoured.
- **Persona seeding at boot instead of per-call.** Considered:
  bake `prompts/persona/*.md` into a persona blob once at startup
  rather than re-compose per phase. Deferred — `ContextBuilder`
  already reads a static field, so the caching is one line if the
  hot path shows up in profiles.

**Don't revert** without explaining what the replacement gives back
that the strict protocol takes away. The whole point of D-025 is
moving format decisions from prose in a system prompt to code in
the loop; re-introducing a "wrap text as a plan" or "accept prose
as the step result" or "trust the model to fence code" clause is
re-introducing the failure mode.

---

## D-026 — System prompt carries persona, not memory; recall is a tool call

**Status:** Accepted · **Date:** 2026-07-04 · Companion to D-025.

**Decision.** The system prompt every LLM call receives is a fixed,
turn-local package:

1. Persona (`prompts/persona/*.md` concatenated) — identity, tone,
   behaviour, output rules, safety.
2. Workspace areas (if configured) — boot-time host-file config.
3. Turn context — integration/channel/thread/user tags.
4. `<memory>` hint block — a fixed reminder that names the memory
   paths and the `memory.read` / `memory.search` / `memory.list`
   tools. **The block does not carry any memory content.**
5. Session summary — the compressed record of the current
   conversation, still pre-injected.

Removed from the system prompt: pre-injection of `shared/INDEX.md`,
the user's `INDEX.md`, `user.md`, and `preferences.md`. Under D-024
these were auto-dumped into every LLM call's system prompt. Under
D-026 the model tool-calls to fetch them when needed.

**Why.** The pre-injection served two purposes: give the planner
enough index visibility to plan sensible lookup steps, and give the
executor an ambient reminder of what's stored. In practice it did
neither cleanly. The system prompt grew unboundedly with each new
INDEX entry, prefix-cache benefits shrank, and — worse — the model
sometimes answered from the pre-injected snapshot without checking
the actual file, which drifted stale. Making recall an explicit
tool call makes the read auditable, keeps the prompt small, and
forces the model to fetch fresh bytes every time.

**How the model still knows what to look for.**

- The persona (`prompts/persona/02-behaviour.md`) tells it: nothing
  is in the prompt, look before denying, start at
  `memory.read({path: "INDEX.md", scope: "user"})`.
- The planner (`prompts/planner.md`) plans lookup steps — when the
  request plausibly touches memory, step one is "read the user
  index" with `tools: ["memory.read"]` or "search memory for X"
  with `tools: ["memory.search", "memory.read"]`.
- The `<memory>` hint block in every system prompt names the tools
  and default scopes so the model doesn't need to guess argument
  shapes.

**What "continues the chat" across turns.** The session summary
(compressed by the summariser after each turn, still injected) plus
the in-memory + on-disk ring buffer of recent messages (D-016) are
the conversational continuity. Persona defines the character;
session state carries what's been said. Neither includes stored
knowledge — that's fetched on demand.

**Turn-count impact.** For turns that need memory recall, expect
one or two extra tool calls (INDEX read + specific file read) inside
the relevant step. Under D-024 those bytes were pre-injected for
free; under D-026 they cost tool calls. Trade accepted because:
prompt size stays bounded, cache prefixes stay stable, model
behaviour is more auditable, and stale-snapshot bugs go away.

**Trivial-input handling.** Greetings still get a one-step plan
with empty tools; executor skips the LLM call; synth composes from
persona alone. No change on the trivial path.

**Cost.**

- One field removed from `ContextBuilder` (`Memory *sandboxfs.FS`
  is now unused there). `main.go` still passes the sandbox to the
  `memory.*` tool pack — nothing else changes.
- Prompt surface shrinks. Every LLM call's system prompt drops
  the four auto-injected file bodies. Small memory footprint;
  larger prefix-cache reuse across turns.
- Extra tool calls per memory-touching turn. Bounded by the
  executor's `PLAN_MAX_STEPS_PER_STEP` (default 4) and
  `PLAN_MAX_STEPS_TOTAL` (default 12) budgets.

**Invariants preserved.**

- Native tool-use (D-001), sandboxed FS (D-003), serial worker
  (D-005), idle rotation / persistence (D-011 / D-016), per-user
  memory layout (D-013), prefix-cache contract (D-017), workspace
  areas (D-019), strict tool-call output (D-025).
- Section ordering (persona → workspace → context → memory hint →
  summary → recent → new user) preserves prefix-cache reuse: the
  system prompt no longer grows with memory content, so the KV
  prefix is stable across turns until persona or workspace change.

**Explicitly not built.**

- **Persona seeded once at session start, then omitted.** LLMs are
  stateless per call; every call needs the persona in the system
  prompt. What the user gets that's "seeded at session start" is
  the persona-defined character; how it's delivered on the wire is
  every-call.
- **Selective memory pre-injection via a size cap.** Considered:
  auto-inject INDEX.md if it's under N bytes. Rejected — a size
  cap re-introduces the "sometimes it's in context, sometimes not"
  branch that made model behaviour hard to predict.
- **Removing the session summary.** Kept. It's compressed
  conversation state, not stored memory, and it's what makes turn-2
  aware of turn-1 without replaying the whole ring buffer.

**Don't revert** without explaining how the replacement (a) keeps
the system prompt bounded across an unbounded-size memory tree,
(b) makes memory reads auditable, and (c) avoids the stale-snapshot
bug class.

---

## D-027 — Per-message turns; no session ring buffer, no summariser

**Status:** Accepted · **Date:** 2026-07-04 · Supersedes D-011
(idle rotation of session summaries) and D-016 (recent-ring
persistence). Complements D-025 / D-026.

**Decision.** Each envelope is a standalone turn. There is no
in-memory session ring buffer, no `recent.json` mirror, no rolling
per-channel summary in `current.md`, no summariser LLM call, no
janitor sweeping idle sessions. Deleted from the tree:

- `internal/agent/session.go`, `internal/agent/session_test.go`
- `internal/agent/summarizer.go`
- `internal/scheduler/janitor.go`,
  `internal/scheduler/janitor_reporter.go`,
  `internal/scheduler/janitor_test.go`
- `prompts/summarizer.md`

`ContextBuilder.ComposeTranscript` now returns a one-element message
list containing only the current envelope's `user` turn.
`ContextBuilder.Persona` is emitted at the top of every phase's
system prompt (planner, executor, synth) so tobee's identity is
present at every LLM call.

The only cross-turn persistence is `data/memory/`, reached
exclusively via the `memory.*` tool pack — which is now the
mechanism the model must use for anything it wants to remember.

**Why.**

- **Poisoned transcripts.** Real traffic produced a session ring
  buffer full of stale plan-announcement text, prior-turn
  `PROTOCOL VIOLATION` nudges, empty assistant messages from
  failed retries, and (when the user re-typed a message that
  looked stuck) the same user message duplicated across positions.
  Every subsequent LLM call was making decisions from that garbage.
- **The summariser was best-effort but load-bearing.** Its rolling
  summary was the only compressed record of the conversation. When
  it hallucinated a fact (e.g. "the French fry recipe has been
  created and saved" *before* the recipe was written), that
  fabrication propagated into every future turn's system prompt
  and the model then acted as if the false state were real.
- **Session boundaries were the wrong scope.** Sessions were
  keyed on `(integration, channel, thread)`, which conflates
  distinct users in a shared channel and creates cross-user
  contamination in the ring buffer.
- **Prefix cache never actually reused.** The system prompt grew
  each turn (new summary + new recent-tail), so the KV cache prefix
  changed on every call. With this change the system prompt is
  byte-stable across turns for a given `(user)` pair.

**What "context across tool calls is fine, context across turns is
not" means concretely.**

- Within one turn: the executor's per-step ReAct sub-loop builds
  `Turn.Transcript` — user question + assistant tool_calls + tool
  results + `step.finish`. That transcript is grown by the executor
  and used by subsequent iterations *within the same step*. Fine.
- Across turns: nothing. Turn N sees only the user's message at
  turn N. Anything else it needs must come from `memory.*` tools.

**Cost.**

- Trivial per-turn: no long-term working memory. If the user says
  "and make it spicy" without repeating context, the model has no
  idea what "it" refers to. Mitigation: the model is instructed
  (via the persona + memory hint) to write anything worth
  remembering to memory files before ending the turn.
- LLM calls per turn: 3 on the happy path (planner + executor +
  synth). Was 4 (added summariser). Cheaper.
- Persona files are now byte-stable across every LLM call for a
  given user, so LM Studio's prefix cache actually reuses.
- Removed env vars: `SESSION_TTL`, `SESSION_IDLE_TIMEOUT`,
  `SESSION_IDLE_TIMEOUT_DM`. Existing configs mentioning them are
  ignored on startup.
- `data/sessions/` is no longer read or written. Old contents on
  disk are inert; delete them manually if desired.

**Invariants preserved.**

- Native tool-use (D-001), sandboxed FS (D-003), serial worker
  (D-005), per-user memory layout (D-013), dynamic scheduled jobs
  (D-015), prefix-cache contract (D-017), workspace areas (D-019),
  strict tool-call protocol (D-025), tool-fetched memory recall
  (D-026).
- Four-phase plan → announce → execute → synth shape (D-024).
- Emoji reaction lifecycle: ✅/🧠/💭 progress markers, cleared on
  success, ❌ on terminal failure (added at deliver() only).

**Explicitly not built.**

- **Optional session revival.** No env var to bring it back. The
  bug class (poisoned transcripts, hallucinated summaries) is
  structural — a flag that re-enables it is a foot-gun waiting.
- **Automatic memory writes for every turn.** The model decides
  what to write. If it forgets to write something worth
  remembering, that's a prompt-tuning issue, not a runtime one.
- **Persona seeded once at chat start.** LLMs are stateless per
  API call; every call needs the full system prompt. The user's
  "seed at start of chat" mental model maps to "system prompt is
  byte-stable across every call," which D-027 achieves — you just
  can't skip re-sending it.

**Don't revert** without explaining how the replacement (a) prevents
the summariser from hallucinating cross-turn state, (b) keeps ring
buffers from filling with stale nudges after a failed turn, and (c)
scopes conversation state correctly for group channels with mixed
users. If you want continuity that survives across messages, do it
through `memory.*` writes and reads — the whole point of D-027 is
that the tool pack is the persistence layer.

---

## D-028 — `prompts/persona/` → `prompts/system/`; tools catalogue becomes a static file

**Status:** Accepted · **Date:** 2026-07-04 · Extends D-018
(persona-file layout) with the folder rename and the static tools
file.

**Decision.**

1. Rename `prompts/persona/` → `prompts/system/`. The folder holds
   the *system prompt* the LLM sees on every phase call — identity,
   tone, behaviour, output rules, safety, and now the tools
   catalogue. Calling it "persona" understated its scope. All five
   original fragments (`00-identity.md` … `04-safety.md`) move
   unchanged.
2. Add `prompts/system/05-tools.md`. Describes every registered tool
   in prose — memory, status, scheduling, workspace (when
   configured), plus the phase-terminating virtual tools
   (`plan.commit`, `step.finish`, `reply.commit`). Sorted last by
   the numeric-prefix load-order contract so it follows the
   behavioural fragments.
3. Delete `renderToolCatalogue` from `internal/agent/planner.go`
   and the `toolCatalogue` field from `Planner`. The `<tools>` block
   the planner previously synthesised at request time from
   `tools.Registry` is gone — the same catalogue prose now lives
   statically in `05-tools.md` and rides in via the system-prompt
   blob.
4. `NewPlanner` no longer takes a `*tools.Registry` argument.
5. `main.go`'s `readPersona` renamed to `readSystemPrompt`. Its
   startup log field `persona_chars` renamed to `system_chars`;
   the "missing" list uses `system/*.md`.

**Why.**

- **Naming honesty.** The folder was already carrying behaviour,
  output rules, and safety instructions — none of which are
  "persona" in the tone-only sense. Calling the whole thing the
  system prompt (which is what it *is* on the wire) removes the
  cognitive rounding.
- **One source of truth for tools.** The runtime catalogue was
  derived from `tools.Registry` and only shown to the planner. The
  executor's per-step `tools=[]` API parameter enforced the real
  constraint. Two derived-from-code sources risked drift and made
  the executor/synth phases blind to the toolkit even though the
  same LLM was reading it three times per turn. A single hand-edited
  file that's part of the system prompt every phase reads keeps the
  description consistent, is discoverable by anyone editing prompts,
  and stops the planner from being the only phase that "knows" what
  tools exist.
- **The tools=[] param is still the enforcer.** Removing the runtime
  render does not weaken security or correctness — the API param
  determines what the model can actually call. The system-prompt
  prose is only guidance.

**Cost / drift risk.**

- If someone adds a new tool to `tools.Registry` without editing
  `05-tools.md`, the model won't see it described. It will still be
  advertised through `tools=[]` when the planner grants it, so a
  clever LLM might still find it — but the intent of D-028 is that
  the tools file is the canonical description. New tool → new
  bullet. Enforcing this by convention, not by a lint.
- Editing `05-tools.md` is a prompt change and requires a
  rebuild-and-restart in prod (the bind mount is gone as of the
  previous commit). Acceptable — tool additions are rare and
  intentional.

**Invariants preserved.**

- Numeric-prefix load-order contract (D-018): filenames sort
  lexicographically; the numbers make the order explicit.
- Every phase sees the same system prompt (D-025 / D-026 / D-027).
- Docker/compose deployment: the folder rename is directory-level,
  so `COPY prompts ./prompts` in the Dockerfile and the dev compose
  bind mount both keep working with no change.

**Explicitly not built.**

- **Renaming `ContextBuilder.Persona` → `ContextBuilder.System`.**
  The field name still fits — it holds the persona/system blob.
  Renaming ripples through every phase file for no runtime gain.
- **A CI check that fails when `05-tools.md` diverges from
  `tools.Registry`.** Would catch drift, but adds harness cost.
  Revisit if drift bugs actually appear.
- **Loading `05-tools.md` as its own file separate from the persona
  concatenation.** The whole point of the rename is that everything
  in `prompts/system/*.md` is one system-prompt blob. A separate
  slot would rebuild the "N files, N branches" complexity D-018
  killed.

**Don't revert** without explaining where the tool catalogue would
live instead, and how the planner (and every other phase) would see
it without paying the render cost per call.

---

## D-029 — One Conversation per request; state-template phase transitions

**Status:** Accepted · **Date:** 2026-07-04 · Supersedes the
"three separate LLM chats per turn" shape of D-024. Keeps D-025's
strict tool-call protocol at every phase and D-027's per-message
boundary. Complements D-026 (memory-via-tools) and D-028 (folder
rename + static tools file).

**Decision.** A single Discord message → a single `Conversation`
that lives from receipt through delivery. All three phases (planner,
executor per step, synth) append to the same growing `Messages` list;
the system prompt is sent once, at `Messages[0]`, and never resent
as a new chat.

Concrete shape of one request's conversation:

```
0  system   prompts/system/*.md + <workspace_areas> + <context> + <memory>
1  user     render(prompts/state/plan.md, {UserInput})
2  asst     plan.commit({goal, steps})              tools=[plan.commit], tool_choice=required
3  tool     ok (ack for plan.commit)

4  user     render(prompts/state/execute_step.md, {Step 1 of N, ...})
5  asst     tool_call (e.g. memory.read)            tools=all registered + step.finish, tool_choice=required
6  tool     result
...
k  asst     step.finish({result, finished})
k+1 tool    ok

... repeat 4..k+1 for each remaining step (unless finished=true) ...

N   user    render(prompts/state/synthesize.md)
N+1 asst    reply.commit({spoken, artifacts})       tools=[reply.commit], tool_choice=required
N+2 tool    ok
```

Per LM Studio call, `tools=[…]` still changes per phase (only
`plan.commit` on message 2; the full registry + `step.finish` during
execute; only `reply.commit` on the synth call). What changes vs
D-024 is the *conversation* is one continuous list, not three fresh
ones.

**Prompt file layout.**

- `prompts/system/*.md` — the system prompt, unchanged. Loaded once.
- `prompts/state/plan.md` — user-message template rendered at
  planning time.
- `prompts/state/execute_step.md` — rendered before each executor
  step.
- `prompts/state/synthesize.md` — rendered before the synth call.
- `prompts/planner.md` and `prompts/synthesizer.md` — **retired.**
  Their operative prose moved into the state templates.

Templates use Go `text/template` with a small `FuncMap` (`join`).
The renderer is `internal/agent/state.go::StateTemplates`.

**Code shape.**

- `Conversation` ([internal/agent/conversation.go](../internal/agent/conversation.go))
  holds `Messages []llm.Message`, `Plan *Plan`, `StepCursor int`,
  `SurfacedKnowledge []string` (stub), `Finished bool`.
- `Turn` owns a `*Conversation` and the per-envelope state
  (message ID, plan-message ID, reactions).
- `Planner`, `Executor`, `Synthesizer` all take a `*StateTemplates`
  and share the same `Conversation` via `Turn`.
- Per-step tool scoping (D-024's `Step.Tools` field) — **removed.**
  Every step's LLM call advertises the full `tools.Registry` + the
  virtual `step.finish` tool. The planner emits `plan.commit` with
  `{goal, steps: [{intent}]}` only.
- `step.finish` schema gained a `finished` boolean. When any step
  sets it, `Conversation.Finished` flips and the loop skips
  remaining steps (marked `⏭️` `StepSkipped`). If the last planned
  step completes without `finished: true`, a `WARN` is logged as
  the correctness surface.

**Why.**

- **The old planner was blind.** In D-024's shape, the planner
  committed a plan without being able to call tools — so the LLM
  planned in the abstract. Real traffic showed it committing plans
  like `[find recipe, format recipe, save recipe]` with empty
  per-step tool grants, which meant the executor skipped every
  step and nothing was actually done. Under D-029, the planner
  still commits a structured plan up front, but every subsequent
  step runs in the *same conversation* and can actually call
  tools with full memory of what the plan intended.
- **Three fresh system prompts per turn was expensive and
  duplicative.** A ~13KB system prompt was sent once per phase.
  Under D-029, LM Studio's KV cache actually reuses the prefix
  because the system message is byte-stable at position 0 for the
  entire request; every LLM call inside the same turn pays
  prefill cost only for the new tail.
- **State templates are editable prompts, not Go string literals.**
  Adding a new phase-transition instruction is a template edit
  and a rebuild, not a `fmt.Sprintf` in Go code.

**Cost / risk.**

- **Self-talk risk.** The synth now sees the full ReAct transcript
  (executor's assistant messages + tool results). D-023's revert
  was precisely because synth continued the transcript instead of
  rendering. Mitigation: `prompts/state/synthesize.md` frames the
  request as "the work is done; render, don't continue" and
  `reply.commit` is forced via `tool_choice=required`. If self-talk
  resurfaces in practice, fall back to synth-sees-only-plan (D-024's
  approach) as a follow-up decision.
- **Growing per-call payload.** Every LLM call sends the whole
  growing conversation. Bounded by budgets: `PLAN_MAX_STEPS_TOTAL`
  (default 12) × per-step tool calls × per-message tokens. Not a
  concern at current scale.
- **State template maintenance.** Prompts drift from what the
  code does if either changes without the other. Same maintenance
  burden as any prompt/code contract — spread across four files
  (`system/*.md` blob + three state templates).

**Invariants preserved.**

- Native tool-use (D-001), sandboxed FS (D-003), serial worker
  (D-005), per-user memory layout (D-013), prefix-cache contract
  (D-017), workspace areas (D-019), strict tool-call output
  (D-025), memory-via-tools (D-026), per-message turn (D-027),
  static tools catalogue file (D-028) — all unchanged.
- Plan announcement + in-place edits (D-024) — unchanged. The
  `plan.RenderAnnouncement` / `plan.RenderStatus` behaviour and
  the emoji lifecycle are preserved. A new `⏭️` marker was added
  for steps skipped by `finished: true`.
- Emoji reaction lifecycle: ✅/🧠/💭 progress, ❌ terminal at
  deliver only.

**Explicitly not built.**

- **Synth sees a slim structured input.** D-024's approach. Kept
  as a fallback if self-talk resurfaces.
- **Streamed tool results back to the user.** Progress today is
  the plan-message edits + reactions.
- **Auto-summarising the conversation mid-turn.** Adds complexity
  we don't need at 12-iteration budgets.
- **Removing the planner phase entirely.** A pure ReAct loop
  without an up-front plan was considered — it would remove
  `plan.commit` and let the executor decide steps on the fly.
  Deferred: the plan announcement is a strong UX signal for
  multi-step tasks, and the plan gives synth a structured record
  of intent.
- **Populating `Conversation.SurfacedKnowledge`.** Stub kept on
  the struct so the future web-search / file-search integration
  has a place to write. Empty today.

**Don't revert** without explaining how you'd (a) prevent the
planner from committing plans with no actionable tool grants, and
(b) recover the prefix-cache reuse that fell out of one system
message at position 0.

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
