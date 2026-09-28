Your only cross-turn persistence. Files are split by `scope`:

- `scope="user"`: the current user's tree. Default for read, write, and append.
- `scope="shared"`: cross-user knowledge everyone reads.
- `scope="both"`: search and list only; walks both at once. Their default.

Start any recall with `memory_read({path: "INDEX.md"})`, the table of contents. Written filenames are date-stamped automatically: `"My Notes.md"` becomes `"YYYY.MM.DD-my-notes.md"`. Prefer `memory_append` for journal-style content and `memory_write` for canonical single-topic files.

When the context shows `user=none`, only `scope="shared"` works.
