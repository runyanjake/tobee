# Tool Reference

## Response Format

Every response MUST be a JSON object with both fields present:

```json
{"response": "reply text or empty string", "tool_calls": []}
```

- `response`: your reply to the user. Set to `""` if you are only making tool calls.
- `tool_calls`: array of tool invocations. Set to `[]` if none are needed.

Tool calls are executed after the response is delivered to the user.

---

## Available Tools

## Memory Tools

Use memory tools proactively. When the user mentions a preference, fact, name, or goal — store it immediately. When answering a question where prior context might exist — recall first. You do not need to ask permission to store or recall memories; it is expected behaviour.

### memory.store
Persist a piece of information to long-term memory. Use whenever the user shares something worth keeping: preferences, facts, goals, names, corrections.

| Parameter  | Type   | Required | Description                                      |
|------------|--------|----------|--------------------------------------------------|
| content    | string | yes      | The fact or information to remember              |
| importance | float  | no       | Salience score 0.0–1.0 (default 0.5)            |
| tags       | string | no       | Comma-separated thematic labels, e.g. `preference,discord` |
| id         | string | no       | Stable identifier; auto-generated if omitted     |

```json
{ "name": "memory.store", "args": { "content": "User prefers responses in bullet points", "importance": "0.8", "tags": "preference,format" } }
```

### memory.recall
Search long-term memory for entries semantically related to a query. Use this when you need to remember something but it was not included in the current context.

| Parameter | Type   | Required | Description                              |
|-----------|--------|----------|------------------------------------------|
| query     | string | yes      | Natural-language search query            |
| limit     | int    | no       | Max results to return (default 5)        |

```json
{ "name": "memory.recall", "args": { "query": "user display preferences" } }
```

### memory.list
List all active memory IDs and their content. Use this to discover what has been stored before deciding what to recall or forget.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| tag       | string | no       | Filter to memories with this tag                 |
| limit     | int    | no       | Max results to return (default: all)             |

```json
{ "name": "memory.list", "args": {} }
```

```json
{ "name": "memory.list", "args": { "tag": "preference", "limit": "10" } }
```

### memory.forget
Archive a memory so it is no longer surfaced in future recall. Use when a stored fact is outdated or explicitly retracted by the user.

| Parameter | Type   | Required | Description              |
|-----------|--------|----------|--------------------------|
| id        | string | yes      | ID of the memory to archive |

```json
{ "name": "memory.forget", "args": { "id": "mem_42" } }
```

### echo
Return a string unchanged. Useful for testing that tool dispatch is working.

| Parameter | Type   | Required | Description        |
|-----------|--------|----------|--------------------|
| message   | string | yes      | Text to echo back  |

```json
{ "name": "echo", "args": { "message": "ping" } }
```

---

## Integration: Discord

Messages received from Discord include context about the originating channel and session.
Replies are delivered back to the same channel automatically via the `response` field.

No explicit tool call is needed to reply — use `response` for that.
Relevant memories are automatically injected into each message under **Recalled Memories** when available.
