# Memory

All of tobee's long-term state is plain text files under `data/memory/`.
Readable with `cat`, diff-able in git (though `data/` is gitignored),
editable by hand. No DB, no vector store, no SQLite.

## Taxonomy

Modelled loosely on CoALA (working / episodic / semantic / procedural):

| Kind        | Lives at                                   | Written by                             |
|-------------|--------------------------------------------|----------------------------------------|
| Working     | not persisted                              | in-memory during a turn                |
| Episodic    | `data/sessions/<int>/<chan>/current.md`    | the summarizer after each turn         |
| Semantic    | `data/memory/user.md`, `data/memory/facts/*.md` | LLM tool calls (`memory.write/append`) |
| Procedural  | `data/memory/preferences.md`, `data/memory/feedback/<date>-<slug>.md` | LLM tool calls + user corrections |

## File layout

Memory is split into two top-level slices: **shared** (cross-user knowledge)
and **users** (one tree per individual). See [DECISIONS.md](DECISIONS.md)
D-013 for the rationale.

```
data/memory/
├─ shared/                          # cross-user knowledge
│  ├─ INDEX.md                      # always-injected (shared)
│  └─ facts/
│     └─ <topic>.md
└─ users/
   └─ <integration>/                # e.g. discord
      └─ <user-id>/                 # sanitized, e.g. "12345"
         ├─ INDEX.md                # always-injected (this user)
         ├─ user.md                 # stable profile for this user
         ├─ preferences.md          # how tobee should behave with this user
         ├─ facts/
         │  └─ <topic>.md
         └─ feedback/
            └─ <YYYY-MM-DD>-<slug>.md
```

The user key derives from the inbound `Envelope` (`Integration` + `User`),
sanitized to filesystem-safe characters by `internal/scope`. A scheduler
tick (no user attached) can read/write `shared/` but writes to `user`
scope return a clear error.

**`data/` is gitignored entirely.** Each developer's memory is private.
Fresh clones start empty; the code handles missing files gracefully (the
context builder simply omits absent sections).

Session summaries live in a parallel tree under `data/sessions/` — see
[AGENT.md](AGENT.md#sessions). Sessions are scoped by channel, not user,
because group channels mix users.

## Retention

- `data/memory/` — **persists forever.** Long-term memory is never auto-deleted.
- `data/sessions/` — managed by the in-process janitor
  ([internal/scheduler/janitor.go](../internal/scheduler/janitor.go)). Per
  channel: `current.md` is the rolling summary, `recent.json` is the exact
  ring buffer mirrored on every turn so warm context survives restart.
  When a session has been idle past its threshold —
  `SESSION_IDLE_TIMEOUT` (default 4h) for channels,
  `SESSION_IDLE_TIMEOUT_DM` (default 168h) for DMs — `current.md` is
  rotated to `archive/<timestamp>.md`, `recent.json` is removed, and the
  in-memory session is reset. Archive files are deleted once their mtime
  exceeds `SESSION_TTL` (default 7 days); empty directories are removed
  with them. See [DECISIONS.md](DECISIONS.md) D-010 / D-011 / D-016.

## Tools

Exposed via the tool registry (see [INTEGRATIONS.md](INTEGRATIONS.md)):

| Tool             | What it does                                                 | Default scope |
|------------------|--------------------------------------------------------------|---------------|
| `memory.read`    | Read a file under the selected scope.                        | `user`        |
| `memory.write`   | Create or overwrite a file.                                  | `user`        |
| `memory.append`  | Append to a file, creating it if needed.                     | `user`        |
| `memory.search`  | Case-insensitive substring walk; hits are scope-labelled.    | `both`        |
| `memory.list`    | List files under the selected scope(s).                      | `both`        |

Every tool takes an optional `scope` arg: `"user"`, `"shared"`, or (for
search/list only) `"both"`. The active user identity is read from the
per-turn `context.Context` via `internal/scope`; the `memory.FS` itself
stays user-agnostic and only sees relative paths inside `data/memory/`.

Implementations: [internal/tools/memory/tools.go](../internal/tools/memory/tools.go).
The underlying typed FS: [internal/memory/fs.go](../internal/memory/fs.go).
Scope plumbing: [internal/scope/scope.go](../internal/scope/scope.go).

## Safety

- **Path sandbox.** Every path passed to `memory.FS` methods is validated:
  relative paths only, no `..`, no absolute paths, no Windows volume
  prefixes. Resolved paths are re-checked with `filepath.Rel` to catch
  symlink-style escapes. Non-obvious and easy to break — don't skip the
  `resolve()` helper when adding new FS ops.

- **Size caps.** `memory.MaxFileSize` (currently 64 KB) caps writes and
  appends. Oversized content is rejected at the FS layer; the tool
  surfaces the error back to the model.

- **Data, not instructions.** Memory content injected into the prompt is
  wrapped in `<memory path="...">...</memory>` fences, and the persona
  tells the model to treat it as data. A malicious or careless memory
  entry should not be able to override the persona. This mitigation is
  imperfect; be wary of tools that quote memory content back to the model
  without framing.

- **No binary files.** The FS will happily write them, but the search tool
  treats everything as lines of text. Don't write binaries to `data/memory`.

## Read paths

The model has two ways to see memory:

1. **Always-injected sections** in the system prompt (INDEX, user profile,
   preferences). These are small and stable. See
   [AGENT.md](AGENT.md#context-building) for the full section order.

2. **Tool calls** for everything else. The model consults INDEX.md, then
   uses `memory.search` or `memory.read` to pull specific files on demand.

This is deliberately different from a vector-search design. We hand the
model a curated index + a grep tool, and let it decide what it needs. Works
for a corpus in the low hundreds of files and has the nice property of
producing interpretable recall.

## Write paths

Memory changes only through the `memory.*` tools. The agent is instructed
(via `prompts/personality/02-behaviour.md`) to save meaningful facts proactively — the
bias is toward `memory.append` for journals and lists, `memory.write` for
small canonical files.

There is **no human-in-the-loop approval** for writes. The agent writes
what it judges useful. This is a trust trade-off: some noise, but no
friction. Adjust the persona if the signal-to-noise drops.

## INDEX.md

The memory index is **human-maintained by default**. The agent can read and
search it, and can `memory.append` to it, but wholesale rewrites should be a
deliberate human act. This avoids drift where the model silently deletes or
reorganises the table of contents.

This is a soft convention — there's no enforcement at the tool layer. If
the persona changes to allow the agent to rewrite the index, make sure the
tradeoff is logged in [DECISIONS.md](DECISIONS.md).

## What's not built, and why

- **No vector search.** The corpus is small. Grep + INDEX.md works. Add
  vectors if and when recall measurably fails.
- **No reflection / consolidation cron.** The scheduler hook exists in
  [internal/scheduler/](../internal/scheduler/); nothing is wired. Would
  be a good next feature once signal appears.
- **No auto-promotion from episodic → semantic.** The agent writes to
  `facts/` explicitly when it judges a fact is stable. No background job
  mines sessions for them.

See [DECISIONS.md](DECISIONS.md) for the rationale on each.
