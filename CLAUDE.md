# tobee — Claude Code project context

A personal AI assistant (Go service) that talks to users via Discord, thinks
with a local LLM served by LM Studio, and keeps its memory in plain text
files under `data/`.

**Entry points:** [cmd/tobee/main.go](cmd/tobee/main.go) wires everything. [internal/agent/loop.go](internal/agent/loop.go) is the serial worker that drives a turn.

## Project docs

Detail lives in [.claude/](.claude/). Pick what you need:

- [.claude/README.md](.claude/README.md) — index of the other docs
- [.claude/ARCHITECTURE.md](.claude/ARCHITECTURE.md) — shape of the repo, diagram, layout
- [.claude/AGENT.md](.claude/AGENT.md) — loop, context builder, sessions, summarizer
- [.claude/MEMORY.md](.claude/MEMORY.md) — memory taxonomy, files, tools, safety
- [.claude/INTEGRATIONS.md](.claude/INTEGRATIONS.md) — integration & tool contracts
- [.claude/DECISIONS.md](.claude/DECISIONS.md) — settled design decisions, open questions
- [.claude/CONVENTIONS.md](.claude/CONVENTIONS.md) — rules for working in this repo

Read `CONVENTIONS.md` before writing code. Read `DECISIONS.md` before
proposing a design change.
