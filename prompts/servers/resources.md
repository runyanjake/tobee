The one way to read anything: memory files, workspace files, anything a connected server exposes.

Read part of a file rather than all of it when you know where to look — add #L34-55, or #L34-55,L80-100 for several spans at once. memory_grep and workspace_grep print the line number of every match, which is where those spans come from.
