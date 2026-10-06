Plain-text memory. scope "user" is this person's files, "shared" is everyone's. Start with INDEX.md.

Looking something up works in two passes, coarse then fine:
1. Find the file. memory_grep count=true gives one row per file with its match count, so the heaviest file is usually the right one. memory_list name="Shopping List" finds a file by its name alone.
2. Read the part that matters. memory_grep context=3 shows the matching lines in place; or take the line numbers it printed and read that span with resources_read and a #L.. fragment.

The pattern is literal text unless you set regexp. loose=true lets a phrase match any spelling of it, so "shopping list" also finds shopping_list. Narrow with dir once you know where you are. Reach for a whole file only when you need all of it.

.tobee/ is tobee's own area: the conversation record and what it learned. Readable, never writable. Saved conversations stay out of a search unless you pass history=true — they record what was said, not where facts live.
