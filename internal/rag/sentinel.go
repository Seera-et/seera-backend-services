package rag

import "strings"

// InsufficientSourcesSentinel is what the model writes when the retrieved
// provisions do not answer the question.
//
// A sentinel rather than a guess: "the sources do not contain the requirements
// for obtaining a business licence" is a well-cited, perfectly grounded
// sentence, and nothing in the citation machinery can tell it apart from an
// answer. Without a signal from the one party that knows — the model — that
// reply reaches the reader wearing a "Grounded" badge, which is exactly
// backwards.
//
// Stripped before the reader sees it, and deliberately free of brackets: the
// marker filter buffers on "[", and a sentinel that collided with that would be
// a bug in two places at once.
const InsufficientSourcesSentinel = "NOT_IN_SOURCES"

// separators are what the model might put between the sentinel and the answer.
const sentinelSeparators = " \t\r\n:.-—"

// prefixGate holds back the start of a stream until it knows whether it begins
// with a sentinel.
//
// It buffers only as far as the sentinel is long, so the cost to
// time-to-first-token is a few characters, and the reader never sees a control
// token flash on screen before it is removed.
//
// Not safe for concurrent use; one gate belongs to one stream.
type prefixGate struct {
	want     string
	buf      strings.Builder
	resolved bool
	matched  bool

	// trimming is set after a match whose separator has not arrived yet. The
	// sentinel can end exactly on a chunk boundary — "NOT_", "IN_", "SOURCES",
	// ": …" — and without this the colon and space that followed it were passed
	// straight through to the reader.
	trimming bool
}

func newPrefixGate(want string) *prefixGate { return &prefixGate{want: want} }

// Write passes text through, holding the first few characters until the
// sentinel is either matched or ruled out.
func (g *prefixGate) Write(text string) string {
	if g.trimming {
		rest := strings.TrimLeft(text, sentinelSeparators)
		if rest == "" {
			// Still only separators. Keep waiting for the answer to start.
			return ""
		}
		g.trimming = false
		return rest
	}
	if g.resolved {
		return text
	}

	g.buf.WriteString(text)
	buffered := g.buf.String()

	// Leading whitespace is the model's, not the sentinel's.
	trimmed := strings.TrimLeft(buffered, " \t\r\n")

	switch {
	case strings.HasPrefix(trimmed, g.want):
		g.resolved, g.matched = true, true
		// Whatever separated the sentinel from the answer goes with it — and if
		// none of it has arrived yet, keep trimming into the next chunk.
		rest := strings.TrimLeft(strings.TrimPrefix(trimmed, g.want), sentinelSeparators)
		g.trimming = rest == ""
		return rest

	case len(trimmed) >= len(g.want):
		// Long enough to be sure it is not there.
		g.resolved = true
		return buffered

	case !strings.HasPrefix(g.want, trimmed):
		// Diverged already; no need to hold any longer.
		g.resolved = true
		return buffered

	default:
		// Still could be the sentinel. Hold.
		return ""
	}
}

// Close releases anything still held at the end of a stream.
func (g *prefixGate) Close() string {
	if g.resolved {
		return ""
	}
	g.resolved = true
	return g.buf.String()
}

// Matched reports whether the stream began with the sentinel.
func (g *prefixGate) Matched() bool { return g.matched }
