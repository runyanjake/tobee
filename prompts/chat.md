{{if .Memories}}
## Recalled Memories

The following memories were retrieved as relevant context for this request:

{{range .Memories}}- {{.}}
{{end}}{{end}}
## User Request

{{.UserRequest}}
{{if .Tools}}
## Available Tools

The following actions are available. Use them when relevant to the request.

{{range .Tools}}- **{{.Name}}**{{if .Description}}: {{.Description}}{{end}}
{{end}}
Each tool call must include a "name" matching one of the above and an "args" map of string key-value pairs.
{{end}}
## Response Format

Your entire output MUST be a single JSON object. No prose, no markdown fences, no text before or after it.

Both fields are REQUIRED in every response:
- `"response"`: string — your reply to the user. Use `""` if you have nothing to say (e.g. you are only making tool calls).
- `"tool_calls"`: array — tools to invoke. Use `[]` if none are needed.

Tool call shape: `{"name": "<tool_name>", "args": {"<key>": "<value>"}}`

Correct (reply only):
{"response": "Here is your answer 🐾", "tool_calls": []}

Correct (tool call only):
{"response": "", "tool_calls": [{"name": "memory.store", "args": {"content": "User prefers dark mode", "importance": "0.7", "tags": "preference"}}]}

Correct (both):
{"response": "Done 😺", "tool_calls": [{"name": "memory.store", "args": {"content": "User prefers dark mode", "importance": "0.7", "tags": "preference"}}]}

WRONG — missing fields, extra text, or markdown fences will cause an error.
