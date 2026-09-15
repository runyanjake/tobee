# Glossary

| Term | Definition |
|---|---|
| **Ability** | A capability implemented outside the LLM turn. Today: introspection through `internal/abilities`. |
| **Addressed message** | A Discord message tobee will process: a bot mention, raw `<@id>` or `<@!id>`, a reply to the bot, or the whole word `tobee`. Everything else is dropped as ambient chatter. |
| **Announcement** | The `**Working on:** <goal>` checklist message sent after the planner commits steps, then edited as each step changes status. |
| **Area** | See *Workspace area*. |
| **Artifact** | A `{lang, body}` item in `reply.commit`: content handed over rather than said (code, drafts, snippets). Delivered as a fenced block. |
| **Budget** | A hard cap on a turn: wall-clock (`TurnBudget`, 2m), per-step executor calls (`PLAN_MAX_STEPS_PER_STEP`), total executor calls (`PLAN_MAX_STEPS_TOTAL`). |
| **`<context>` tag** | The system-message section stamping `now`, integration, channel, thread, and user for the turn. |
| **Conversation** | The `agent.Conversation` for one request. A single growing `[]llm.Message` shared by all phases, plus the `Plan`. |
| **D-0xx** | A design decision ID. Current ones are in `DESIGN.md#key-decisions`, superseded ones in `IMPLEMENTATION.md`. Cited in code comments; never reused. |
| **Dated filename** | The `YYYY.MM.DD-kebab-name.ext` form that `datedname.Apply` gives every written memory or workspace file. |
| **Deliver** | The last step of a turn: send `Turn.Reply`, then clear reactions (success) or add ❌ (empty reply). |
| **Direct reply / fast path** | A `plan.commit` with zero steps and a `direct_reply`. Delivered right away in one LLM call (D-032). |
| **Envelope** | `integrations.Envelope`, the normalized inbound event every producer publishes onto the bus. |
| **Executor** | The phase that runs each plan step as a ReAct loop until `step.finish`. |
| **`finished`** | The boolean on `step.finish`. `true` means the whole request is satisfied, and the remaining steps are skipped (⏭️). |
| **INDEX.md** | The per-scope memory table of contents the prompts tell the model to read first. Meant to be maintained by a human. |
| **Integration** | A long-running I/O adapter implementing `Name` / `Start` / `Stop`, e.g. `discord`. |
| **JobManager** | `scheduler.JobManager`, which owns model-created jobs (cron or one-shot), their JSON files, and firing. |
| **Memory scope** | `user` (`data/memory/users/<integration>/<id>/`), `shared` (`data/memory/shared/`), or `both` (search and list only). |
| **Misfire policy: skip** | One-shot jobs whose time passed while tobee was down are deleted at boot, not run. |
| **Nudge** | The `PROTOCOL VIOLATION: …` user message appended before the single retry of a phase. |
| **Phase** | One stage of a turn: plan, (announce), execute step, synthesize, deliver. |
| **Phase directive / state template** | A user-role message rendered from `prompts/state/<phase>.md` and wrapped in `<phase name="…">`. Carries that phase's contract. |
| **Plan** | `agent.Plan`: `Goal`, ordered `Steps` (intent, status, result, error), or a `DirectReply`. Exists only for the turn. |
| **Planner** | The phase that commits the plan via `plan.commit`. |
| **Prefix cache** | LLM-server KV cache reuse when consecutive requests share leading tokens. Why the system message keeps stable sections first and is never rebuilt mid-turn. |
| **Protocol violation** | A phase response without the required tool call (e.g. prose under `tool_choice=required`). Logged at ERROR, nudged, retried once. |
| **ReAct** | Reason + Act: alternate model tool calls and tool results. The executor's inner loop. |
| **Reaction lifecycle** | ✅ received → 🧠 planning → 💭 executing on the inbound message. Cleared on success; ❌ on failure. |
| **Replies** | `agent.Replies`, the integration-keyed table of `ReplySender`, `MessageEditor`, and `Reactor` functions. |
| **`reply.commit`** | The synthesizer's virtual tool: `{spoken, artifacts}`. |
| **Reporter** | `abilities.Reporter`, a subsystem's `Render(ctx, since) → (full, summary)` text for status tools. Current reporters: `discord`, `scheduler`, `schedules`. |
| **Sandbox** | `sandboxfs.FS`, a filesystem rooted at one directory that rejects absolute, volume-qualified, and `..` paths and enforces a size cap. |
| **Scheduled fire** | The envelope a job publishes when it fires. Content `[scheduled fire: <name>] <prompt>`, routed to the creating channel and user. |
| **Scope (`UserScope`)** | The per-turn integration / user / user name / channel / thread attached to `ctx` via `scope.With`. |
| **Serial worker** | The single agent goroutine draining the bus, one turn at a time (D-005). |
| **`spoken`** | The plain-text part of `reply.commit`, in tobee's voice. |
| **Status tools** | `status.summary` and `status.report`. Verbatim tools rendering Reporter output over a relative `window`. |
| **Step** | One planned outcome (`intent`) with status `pending` / `running` / `done` / `failed` / `skipped`. |
| **`step.finish`** | The executor's virtual tool that ends a step: `{result, finished}`. |
| **Synthesizer** | The phase that turns the finished work into the user-facing reply via `reply.commit`. |
| **System prompt** | `prompts/system/*.md` joined in filename order, plus workspace areas, `<context>`, and `<memory>`. Sent once as message 0. |
| **Tick** | An envelope the static `Scheduler` publishes on an interval. None registered today. |
| **Tool pack** | A package under `internal/tools/<pack>/` exposing `Register(reg, deps…)`: `memory`, `workspace`, `schedule`, `status`. |
| **Tool registry** | `tools.Registry`: JSON-Schema specs, timeouts, panic recovery, `Verbatim` flag. |
| **Turn** | `agent.Turn`, all processing for one envelope, from dequeue to deliver. |
| **Verbatim** | `tools.Spec.Verbatim`: the tool's output is appended to the reply by code and never rewritten by the model (D-030). |
| **Virtual tool** | A phase-only tool schema not in the registry: `plan.commit`, `step.finish`, `reply.commit`. |
| **`window`** | The status tools' relative lookback (`30m`, `24h`, `7d`). Default 1h, max 30d. |
| **Workspace area** | An operator-configured sandboxed host directory (`WORKSPACE_AREA_<NAME>`, optional `_DESC`, `_READONLY`) reachable through `workspace.*`. |
