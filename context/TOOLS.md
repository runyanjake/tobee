# Tool Reference

Tobee can invoke tools by including them in the `tool_calls` array of the response JSON.
Both `response` and `tool_calls` are optional — use whichever is appropriate.

## Response Format

```json
{
  "response": "Conversational reply to the user (omit if using tool_calls only)",
  "tool_calls": [
    { "name": "tool_name", "args": { "param": "value" } }
  ]
}
```

`response` is sent back to the user automatically. `tool_calls` are executed in order after the response is delivered.

---

## Available Tools

### set_context
Store a named value in runtime memory. Useful for tracking user preferences, ongoing tasks, or any state that should persist across messages.

| Parameter | Type   | Required | Description          |
|-----------|--------|----------|----------------------|
| key       | string | yes      | Name of the variable |
| value     | string | yes      | Value to store       |

```json
{ "name": "set_context", "args": { "key": "user_name", "value": "Alice" } }
```

### get_context
Retrieve a previously stored value from runtime memory.

| Parameter | Type   | Required | Description          |
|-----------|--------|----------|----------------------|
| key       | string | yes      | Name of the variable |

```json
{ "name": "get_context", "args": { "key": "user_name" } }
```

### clear_context
Remove a stored value from runtime memory.

| Parameter | Type   | Required | Description              |
|-----------|--------|----------|--------------------------|
| key       | string | yes      | Name of the variable to remove |

```json
{ "name": "clear_context", "args": { "key": "user_name" } }
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
Tool calls from this integration are for side-effects: storing context, triggering actions, etc.
