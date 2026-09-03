package rag

import (
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// Conversation limits. A chat can run long; what reaches the model must not.
const (
	// MaxHistoryTurns is how many earlier turns the API accepts. Beyond this the
	// oldest are dropped — a legal follow-up depends on the last exchange or two,
	// not on the whole session.
	MaxHistoryTurns = 8

	// MaxTurnRunes truncates one earlier turn, keeping its start and dropping
	// its end.
	//
	// Raised from 1200, 2026-08-31: this comment used to claim "the end of a
	// long answer is rarely what a follow-up refers to" — found live to be
	// false for exactly the answers this system now produces. A structured,
	// multi-section reply (rule 13 in grounding.go asks for these) is
	// routinely referenced by its later sections — "under B", "the third
	// point" — and 1200 runes cut a real ~3,800-rune answer off before its
	// "B." section, so a follow-up asking about it had no prior content to
	// resolve the reference against. Kept equal to router.maxTurnRunes: the
	// two must agree on how much prior content exists, or routing can miss a
	// reference generation would have understood, which is what happened
	// here.
	MaxTurnRunes = 4000
)

// Turn is one earlier exchange, supplied by the client.
//
// The server stores no conversation: each request carries the history it wants
// considered. That keeps the "no answer text is persisted" property of the
// backend intact and means a conversation belongs to whoever holds it, which
// matters while there is no authentication.
type Turn struct {
	Role ai.Role
	Text string
}

// TrimHistory bounds what the caller supplied: the most recent turns, each cut
// to length. Applied server-side so a client cannot enlarge the context window
// by sending more.
func TrimHistory(history []Turn) []Turn {
	if len(history) > MaxHistoryTurns {
		history = history[len(history)-MaxHistoryTurns:]
	}

	out := make([]Turn, 0, len(history))
	for _, turn := range history {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		if turn.Role != ai.RoleAssistant {
			turn.Role = ai.RoleUser
		}
		turn.Text = truncateRunes(text, MaxTurnRunes)
		out = append(out, turn)
	}
	return out
}

// writeHistory renders earlier turns for a prompt, oldest first and bounded.
func writeHistory(b *strings.Builder, history []Turn) {
	for _, turn := range history {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		if turn.Role == ai.RoleAssistant {
			b.WriteString("Assistant: ")
		} else {
			b.WriteString("User: ")
		}
		b.WriteString(truncateRunes(text, MaxTurnRunes))
		b.WriteString("\n\n")
	}
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}
