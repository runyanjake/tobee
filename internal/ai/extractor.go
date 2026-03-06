package ai

import "strings"

// ResponseExtractor progressively extracts the "response" string value from a
// streaming JSON payload that may begin with a <think>...</think> block.
//
// Call Feed for each token received from the stream; it returns whatever new
// characters have been extracted from the "response" value so far (empty string
// if nothing new is available yet).
type ResponseExtractor struct {
	buf     strings.Builder // raw accumulator for think/search phases
	out     strings.Builder // accumulates extracted response chars
	phase   extractPhase
	escaped bool // previous char was backslash inside JSON string
}

type extractPhase int

const (
	phaseThink     extractPhase = iota // waiting for </think> end tag
	phaseSearch                        // looking for `"response"` key
	phaseSkipColon                     // skipping whitespace/colon before opening quote
	phaseString                        // inside the response string value
	phaseDone                          // extraction complete
)

// Feed accepts the next token from the AI stream and returns any newly
// extracted response characters. Call Result() when the stream ends to get
// the full extracted value.
func (e *ResponseExtractor) Feed(token string) string {
	if e.phase == phaseDone {
		return ""
	}

	startLen := e.out.Len()

	for _, ch := range token {
		e.processChar(ch)
		if e.phase == phaseDone {
			break
		}
	}

	return e.out.String()[startLen:]
}

func (e *ResponseExtractor) processChar(ch rune) {
	switch e.phase {
	case phaseThink:
		e.buf.WriteRune(ch)
		s := e.buf.String()
		if idx := strings.LastIndex(s, "</think>"); idx >= 0 {
			remainder := s[idx+len("</think>"):]
			e.buf.Reset()
			e.buf.WriteString(remainder)
			e.phase = phaseSearch
			// Process accumulated remainder through search phase
			e.scanForResponseKey()
		}

	case phaseSearch:
		e.buf.WriteRune(ch)
		e.scanForResponseKey()

	case phaseSkipColon:
		// Skip whitespace and the colon; opening quote transitions to string phase
		switch ch {
		case ' ', '\t', '\n', '\r', ':':
			// skip
		case '"':
			e.phase = phaseString
		}

	case phaseString:
		if e.escaped {
			e.escaped = false
			switch ch {
			case 'n':
				e.out.WriteRune('\n')
			case 't':
				e.out.WriteRune('\t')
			case 'r':
				e.out.WriteRune('\r')
			default:
				e.out.WriteRune(ch)
			}
			return
		}
		switch ch {
		case '\\':
			e.escaped = true
		case '"':
			e.phase = phaseDone
		default:
			e.out.WriteRune(ch)
		}
	}
}

// scanForResponseKey checks if the buffer contains `"response"` and advances
// the phase if found.
func (e *ResponseExtractor) scanForResponseKey() {
	s := e.buf.String()
	if idx := strings.Index(s, `"response"`); idx >= 0 {
		// Feed remaining chars after the key through phaseSkipColon one by one
		e.buf.Reset()
		e.phase = phaseSkipColon
		for _, ch := range s[idx+len(`"response"`):] {
			e.processChar(ch)
			if e.phase == phaseDone {
				break
			}
		}
	}
}

// Result returns the full extracted response value accumulated so far.
func (e *ResponseExtractor) Result() string {
	return e.out.String()
}
