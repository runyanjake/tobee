package discord

import "strings"

const maxMessageLen = 2000

// splitMessage fits Discord's length cap, preferring natural breaks in bestSplit's order.
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

func bestSplit(s string, max int) int {
	candidates := []string{
		"```\n",
		"\n\n",
		". ",
		"! ",
		"? ",
		"\n",
	}
	for _, sep := range candidates {
		if idx := lastIndex(s, sep, max); idx > 0 {
			return idx + len(sep)
		}
	}
	return max
}

func lastIndex(s, sep string, limit int) int {
	if limit > len(s) {
		limit = len(s)
	}
	return strings.LastIndex(s[:limit], sep)
}
