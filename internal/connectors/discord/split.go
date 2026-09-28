package discord

import "strings"

const maxMessageLen = 2000

// splitMessage breaks s into chunks of at most maxMessageLen characters,
// preferring to split after closing code fences, then paragraph breaks,
// then sentence endings, then newlines, falling back to a hard cut.
func splitMessage(s string) []string {
	if len(s) <= maxMessageLen {
		return []string{s}
	}

	var chunks []string
	for len(s) > maxMessageLen {
		cut := bestSplit(s, maxMessageLen)
		chunks = append(chunks, strings.TrimRight(s[:cut], " "))
		s = strings.TrimLeft(s[cut:], " ")
	}
	if s != "" {
		chunks = append(chunks, s)
	}
	return chunks
}

// bestSplit returns the index at which to split s, no greater than max.
func bestSplit(s string, max int) int {
	candidates := []string{
		"```\n", // after a closing code fence
		"\n\n",  // paragraph break
		". ",    // sentence end
		"! ",
		"? ",
		"\n", // any newline
	}
	for _, sep := range candidates {
		if idx := lastIndex(s, sep, max); idx > 0 {
			return idx + len(sep)
		}
	}
	return max
}

// lastIndex returns the start of the last occurrence of sep in s[:limit].
func lastIndex(s, sep string, limit int) int {
	if limit > len(s) {
		limit = len(s)
	}
	return strings.LastIndex(s[:limit], sep)
}
