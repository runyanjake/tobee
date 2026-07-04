# Conventions

Rules for working in this repo. Read before writing code.

## Scope discipline

- **Don't add features outside the design without discussion.** Features
  deferred in [DECISIONS.md](DECISIONS.md) (vector search, reflection,
  MCP, streaming, provider abstraction) are deferred on purpose. If you
  think one is worth building now, say so before writing it.
- **Don't add backwards-compat shims.** No parallel "old way / new way"
  code paths. No fallback from native tool-use to JSON-in-string. No
  conditional vector-vs-grep dispatch. Pick one path and commit.
- **Don't preemptively abstract.** One provider today → one concrete
  `llm.Client`. One memory backend → one `memory.FS`. Introduce an
  interface when the second implementation exists, not before.

## Code style

- **Go idiomatic.** Effective-Go / Uber-style. `gofmt`. No external
  linters configured.
- **Minimal comments.** Explain *why*, not *what*. If a future reader
  would not be confused without the comment, don't write it. Exception:
  package docs and exported-symbol docs when the name isn't obvious.
- **No multi-line comment blocks** describing what a function does — the
  name and signature should. A one-line hint is fine.
- **Error wrapping.** Always `fmt.Errorf("<verb>: %w", err)` at package
  boundaries. Don't shadow errors.
- **Logging.** Use `log/slog`. Structured fields, not interpolated
  strings: `slog.Info("agent: turn begin", "channel", env.Channel)`.
- **Package-prefix messages.** Log messages are prefixed with the
  subsystem (`"agent: …"`, `"discord: …"`, `"memory: …"`). Grep-friendly.

## Memory and filesystem

- **Always go through `memory.FS`.** Never `os.Open` / `os.WriteFile`
  against a path inside `data/memory/`. The sandbox is in `FS.resolve()`;
  reaching past it defeats the safety invariant. See
  [MEMORY.md](MEMORY.md) on safety.
- **Do not commit `data/`.** It's in `.gitignore`. If you find yourself
  wanting to track seed files, re-open the decision first — see
  [DECISIONS.md](DECISIONS.md) D-008.

## Agent loop

- **Serial worker is load-bearing.** Don't parallelise envelope
  consumption without thinking hard about memory-write races and out-of-
  order replies. See [DECISIONS.md](DECISIONS.md) D-005.
- **No cross-turn state.** Each envelope is a standalone turn. Don't
  reintroduce a session ring buffer, a rolling summariser, or any
  channel-scoped conversation state — persistence goes through
  `memory.*` tools instead. See D-027.
- **Step / turn budgets exist for a reason.** Don't remove them as part
  of "making this work for one more case."

## Tools

- **JSON-Schema args.** Not `map[string]string`. New tools provide a
  well-formed `InputSchema`; `registry.Register` will fill a permissive
  default if omitted, but that's a convenience, not a target.
- **Namespace tools by subsystem.** `memory.*`, `web.*`, `calendar.*`.
  Not by integration (`discord.*`). The reply path is not a tool.
- **Tools are for things the model chooses to do.** Implicit behaviours
  (reply delivery, reaction updates) are not tools.

## Integrations

- **Everything is an `Envelope`.** Don't introduce per-integration event
  types. If a new piece of metadata is needed universally, extend
  `Envelope`. If it's integration-specific, stash it in a wrapper local
  to that integration.
- **Inbound = `bus.Publish`. Outbound = registered `ReplySender`.** Don't
  invent new paths.

## Prompts

- **Prompts live in files under `prompts/`** and are loaded at startup.
  Docker mounts `prompts/` as a volume so edits don't need rebuilds, but
  they do need a process restart.
- **Don't hard-code prompt content in Go.** Single exception: trivial
  one-liners used for debugging.

## Docker / ops

- **Docker is a first-class run target.** Every change should still work
  under `docker compose up --build`. Check `host.docker.internal` hasn't
  regressed.
- **The container runs as a non-privileged user.** Don't add operations
  that require root at runtime.
- **`.env` is the config surface.** New config: add to `.env.example`
  with a one-line comment, read via `os.Getenv`.

## Response style (when working with the user)

- **Terse.** Short sentences. Short paragraphs. No trailing summaries of
  what you just did — the diff is visible.
- **Ask before destructive actions.** Deleting files, wiping `data/`,
  force-pushing, rewriting history. Reversible edits are fine; anything
  that loses state gets confirmation.
- **State decisions, don't narrate deliberation.** "Moving X to Y." Not
  "I'm thinking about whether X should be at Y…".

## What to update and when

- **Code changes**: update the code. Docs in `.claude/` usually don't
  need a touch.
- **A design decision changes**: add a new entry to
  [DECISIONS.md](DECISIONS.md). Don't edit the old one.
- **A new convention emerges** (something that bit someone twice): add it
  here.
- **The shape of the repo changes**: update
  [ARCHITECTURE.md](ARCHITECTURE.md).
