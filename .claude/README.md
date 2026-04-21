# .claude — project context for Claude Code

Long-lived context that outlives any one session. These files describe **what
tobee is, why it is shaped this way, and how to work in the codebase**.

They do *not* describe the code in detail — that's what the code is for.
Read a file, open `main.go`, follow the imports.

## Files

| File                 | Read it when…                                                         |
|----------------------|-----------------------------------------------------------------------|
| [ARCHITECTURE.md](ARCHITECTURE.md) | …you need the shape: diagram, packages, data flow, layout.        |
| [AGENT.md](AGENT.md) | …you're touching the loop, context builder, sessions, summarizer. |
| [MEMORY.md](MEMORY.md) | …you're changing how tobee remembers things. Read before adding memory features. |
| [INTEGRATIONS.md](INTEGRATIONS.md) | …you're adding an integration, a tool, or changing how replies get delivered. |
| [DECISIONS.md](DECISIONS.md) | …you want to know *why* something is the way it is, or before proposing a different approach. |
| [CONVENTIONS.md](CONVENTIONS.md) | …before writing or editing code. Style and guardrails. |

`CLAUDE.md` at the repo root is the minimal auto-loaded index that points here.

## Keeping these current

- When a design decision changes, update [DECISIONS.md](DECISIONS.md) — add a new entry, don't silently mutate the old one. We want the history.
- When a new convention emerges (something that tripped someone up twice), add it to [CONVENTIONS.md](CONVENTIONS.md).
- Routine code changes do **not** require a docs edit. These files explain *intent*, not *implementation*.
