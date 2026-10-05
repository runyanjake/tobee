Plain-text memory. scope "user" is this person's files, "shared" is everyone's. Start with INDEX.md.

Looking something up works in two passes:
1. memory_search mode="files" — which files are about this, and which line ranges to read.
2. Either memory_search with context=3 to see the matching lines in place, or resources_read with the #L.. span from the first pass.

Narrow with dir once you know where you are. Reach for a whole file only when you need all of it.

.tobee/ is tobee's own area: the conversation record and what it learned. Readable, never writable.
