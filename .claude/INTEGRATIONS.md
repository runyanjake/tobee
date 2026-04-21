# Integrations & Tools

Two distinct contracts, deliberately kept separate.

- **Integration** = long-running I/O source. Starts on boot, pushes inbound
  events, delivers outbound replies. The agent doesn't invoke it directly.
- **Tool** = request/response callable. Discoverable by the model each
  turn, dispatched by the registry.

An integration may *expose* a tool (e.g. Discord exposes its reply sender
as a registered `ReplySender`), and that's the only overlap.

## Integration contract

[internal/integrations/integration.go](../internal/integrations/integration.go)

```go
type Integration interface {
    Name() string
    Start(ctx context.Context) error
    Stop() error
}
```

Inbound events flow through an `Envelope`:

```go
type Envelope struct {
    Integration string    // "discord", "slack", "scheduler"
    User        string    // stable per-integration user id
    Channel     string    // opaque routing id used when replying
    Thread      string    // optional thread id
    Content     string    // text body
    Received    time.Time
}
```

`Envelope.Key()` derives the session key — `integration:channel[:thread]`.
This is the unit of scope for short-term history and rolling summaries.

## Bus

[internal/integrations/bus.go](../internal/integrations/bus.go) is a
buffered channel. Producers call `Publish`; the single agent consumer
receives via `C()`.

Publish is **non-blocking** — if the buffer is full, the envelope is
dropped with a warning. This is intentional: we'd rather drop than block a
Discord gateway goroutine. If drops become common the agent is
persistently behind, which is a signal the turn budget or loop design
needs revisiting.

## Reply path

The agent delivers final text via an integration-keyed lookup table rather
than a tool call. See [internal/agent/reply.go](../internal/agent/reply.go):

```go
type ReplySender func(ctx context.Context, channel, thread, text string) error

type Replies struct { … }
func (r *Replies) Register(integration string, s ReplySender)
func (r *Replies) Send(ctx, integration, channel, thread, text)
```

Each integration registers its own sender during construction. Discord's is
in [internal/integrations/discord/bot.go](../internal/integrations/discord/bot.go).

Reply text is not modeled as a tool call because it's the default end-state
of a turn, not a thing the model chooses to do. Sending messages to *other*
channels than the one that triggered the turn would be a tool (not built).

## Adding an integration

The steps, roughly:

1. New subdirectory under [internal/integrations/](../internal/integrations/).
2. Implement the `Integration` interface (`Name`, `Start`, `Stop`).
3. On inbound events, construct an `Envelope` and call `bus.Publish`.
4. Register a `ReplySender` on `*agent.Replies` during construction.
5. Wire it in [main.go](../main.go) alongside Discord.

Don't introduce a new event type. Everything is an Envelope.

## Tool registry

[internal/tools/registry.go](../internal/tools/registry.go)

```go
type Spec struct {
    Name        string
    Description string
    InputSchema json.RawMessage // JSON Schema
    Timeout     time.Duration   // 0 = 30s default
    Handler     Handler
}

type Handler func(ctx, args json.RawMessage) (string, error)
```

- **JSON-Schema args**, not `map[string]string`. Matches OpenAI / Anthropic
  native tool-use and is the lingua franca for MCP when we want it later.
- `Call` wraps every handler with a per-tool timeout and a panic recovery.
  One bad tool cannot kill the agent.
- Tool names: lowercase, dot-namespaced (`memory.read`, `memory.search`).
  Namespace by subsystem, not by integration.

### Adding a tool

1. Pick a pack (`internal/tools/<pack>/`) or make a new one.
2. Write a `Register(reg *tools.Registry, deps …)` that calls
   `reg.MustRegister` for each tool.
3. Declare an `InputSchema` as a JSON-Schema object (raw JSON is fine; the
   existing memory tools are good templates).
4. In [main.go](../main.go), call your pack's `Register`.

## MCP — not yet

The registry shape was chosen so that a future `internal/tools/mcp/`
package can act as an **MCP client**: connect to an external MCP server,
translate its `tools/list` response into `tools.Spec` entries, and dispatch
`tools/call` on invocation. Nothing about the registry or loop needs to
change for this to work.

We do not today:

- Run MCP servers inside tobee (no subprocess management).
- Expose tobee's own tools as an MCP server.

Both are additive when demand appears. See [DECISIONS.md](DECISIONS.md)
for why we didn't build them up-front.
