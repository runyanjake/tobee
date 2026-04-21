# tobee

An AI-powered personal assistant named after my family cat Tobee 🐾.

A long-running Go service that talks to users through pluggable integrations
(Discord today), thinks with a local LLM served by
[LM Studio](https://lmstudio.ai/), and keeps its memory in plain text files
under `data/`.

Architecture and design rationale live in [.claude/](.claude/).

## The agent loop

One message at a time goes through a ReAct-style loop:

1. **Context** is assembled from the persona prompt, a memory index, the
   user profile, the rolling session summary, and the last few verbatim
   turns. Built in [internal/agent/context.go](internal/agent/context.go).
2. **LLM call** to LM Studio with the full message list and every
   registered tool's JSON schema. Native function-calling — no home-rolled
   JSON-in-string protocol.
3. **Tool dispatch.** If the model requested tools, they run, results are
   fed back as `role: tool` messages, and step 2 repeats. Up to 8
   iterations or 2 minutes, whichever comes first.
4. **Reply** is delivered through the integration that originated the
   message (Discord, for now).
5. **Summarizer** compresses the turn into a rolling `current.md` per
   session in the background. Best-effort; a failure never blocks a reply.

A single worker goroutine drains the event bus serially, so memory-file
writes never race and replies stay deterministic. See
[.claude/AGENT.md](.claude/AGENT.md) for detail.

## Memory

Everything tobee remembers is a plain text file under `data/`. No DB, no
vector store, no SQLite.

```
data/
├─ memory/
│  ├─ INDEX.md           # always-injected table of contents
│  ├─ user.md            # stable user profile
│  ├─ preferences.md     # how tobee should behave
│  ├─ facts/             # one topic per file (agent-written)
│  └─ feedback/          # dated corrections
└─ sessions/
   └─ <integration>/<channel>/
      └─ current.md      # rolling summary, rewritten each turn
```

The model reaches memory two ways: always-injected files (index, profile,
preferences) appear in every system prompt; everything else is pulled on
demand via the `memory.*` tools (`read`, `write`, `append`, `search`,
`list`). That's deliberate — the model gets a curated index + a grep tool
instead of vector-similarity guesswork.

All paths are sandboxed to `data/memory/`; writes over 64 KB are rejected;
memory content is framed as data, not instructions, in the prompt. See
[.claude/MEMORY.md](.claude/MEMORY.md).

`data/` is gitignored. Each install has its own memory.

## Run

```bash
cp .env.example .env   # fill in DISCORD_TOKEN, point AI_PROVIDER_URL at LM Studio
docker compose up --build
```
