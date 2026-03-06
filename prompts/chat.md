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

You must respond with a single valid JSON object. No markdown fences. No text outside the JSON.

Schema:
- "response": string — your reply to the user
- "tool_calls": array — tools to invoke (use empty array if none needed)

Tool call shape: {"name": "<action_name>", "args": {"<key>": "<value>"}}

Example (no tools):
{"response": "Here is your answer 🐾", "tool_calls": []}

Example (with tool):
{"response": "Done 😺", "tool_calls": [{"name": "set_context", "args": {"key": "mood", "value": "happy"}}]}
