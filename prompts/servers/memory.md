Plain-text memory. scope "user" is this person's files, "shared" is everyone's. Start with INDEX.md.

Looking something up works in two passes, coarse then fine:
1. Find the file. Counting mode gives one row per file with its number of matches, so the heaviest file is usually the right one. Listing can filter by filename instead, which finds a file named after a topic even when its contents never say the words.
2. Read the part that matters. Asking for context lines shows the matches in place; or take the line numbers printed beside them and read just that span with resources_read.

The pattern is literal text unless you ask for a regular expression. Loose matching lets a phrase match any spelling of it, so spaces, hyphens and underscores stand for each other. Narrow to a subdirectory once you know where you are. Reach for a whole file only when you need all of it.

.tobee/ is tobee's own area: the conversation record and what it learned. Readable, never writable. Saved conversations stay out of a search unless you ask for them — they record what was said, not where facts live.
