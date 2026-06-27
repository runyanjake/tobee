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
file under `prompts/personality/`, sorted lexicographically and joined
with blank lines. Day-one fragments:

- `00-identity.md` — who tobee is.
- `01-tone.md` — how it speaks. Matter-of-fact; no pleasantries.
- `02-behaviour.md` — what it does (actions, memory consultation).
- `03-output.md` — format constraints.
- `04-safety.md` — boundaries, memory-as-data framing.

`main.go::readPersonality` does the glob + sort + read. Tool guidance
still lives in tool `Description` fields, per D-006's tool half.

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
— the answer is `prompts/personality/`, full stop. Anyone editing the
persona must remember to drop new fragments in this folder rather than
hard-coding strings in Go. CONVENTIONS.md's "prompts live in files"
rule already covers this.

**Don't revert** without first identifying which file in the folder was
the actual problem. If a fragment is causing drift, edit it. If the
whole split is causing drift, log a new decision.

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
