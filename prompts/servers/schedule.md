Timers you set for yourself. A job fires later as a message back to you, in the same channel and for the same user that created it, prefixed `[scheduled fire: <name>]`. Jobs survive restarts.

- One-shot: `at` is RFC3339 or `"in 10m"`. Recurring: `cron` is 5-field or `@every 30m` / `@daily`.
- Write `prompt` as a directive to your future self ("check the deploy status and report back").
- Keep the job id from `schedule_create` if you may need `schedule_cancel`.
