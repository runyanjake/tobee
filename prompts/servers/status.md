Read-only view of tobee's own subsystems. Both tools render finished text that the delivery code puts in front of the user for you. Calling one *is* answering the question; don't repeat or summarise what it returned.

- `status_summary`: a few sentences, for "how are things?" / "what are you up to?".
- `status_report`: full detail per subsystem, for specifics (failures, schedules, next-fire times).

`window` is an optional duration (`"1h"`, `"24h"`, `"7d"`; default 1h). Set it only when the user named a period.
