package rag

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// Conversation limits. A chat can run long; what reaches the model must not.
const (
	// MaxHistoryTurns is how many earlier turns the API accepts. Beyond this the
	// oldest are dropped — a legal follow-up depends on the last exchange or two,
	// not on the whole session.
	MaxHistoryTurns = 8

	// MaxTurnRunes truncates one earlier turn. Answers are the long ones, and the
	// end of a long answer is rarely what a follow-up refers to.
	MaxTurnRunes = 1200

	// condenseTimeout bounds the rewrite. Time-to-first-token is a product
	// requirement, so a slow rewrite is abandoned rather than allowed to delay
	// the answer; retrieval then falls back to the concatenated form.
	condenseTimeout = 5 * time.Second

	// condenseMaxTokens caps the rewrite. It is one sentence.
	condenseMaxTokens = 120
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

// condensePrompt turns a follow-up into a question retrieval can work on.
//
// It contains no legal content and asks for none: the model is rewriting a
// sentence, not answering it. The output feeds hybrid search — it never reaches
// the user and can never become a legal claim, because generation still sees
// only what retrieval returned.
const condensePrompt = `You rewrite a follow-up question so that it can be understood on its own.

You are given the earlier turns of a conversation and the latest question. Resolve
pronouns and references ("it", "that article", "the second one", "what about…")
into explicit terms taken from the earlier turns.

Rules:
- Output only the rewritten question. No preamble, no quotes, no explanation.
- Do not answer the question.
- Do not add facts, laws, article numbers, or conditions that are not already in
  the conversation.
- Keep the language of the latest question.
- If the latest question already stands on its own, output it unchanged.`

// retrievalQuery returns the text retrieval should search for.
//
// For the first question in a conversation that is the question itself. For a
// follow-up it is a standalone rewrite, because "what about for a PLC?" retrieves
// nothing on its own — the terms that make it searchable are in the previous
// turn.
//
// Every failure path degrades rather than fails: a rewrite that errors, times
// out, or comes back empty falls back to the last user turn joined to the
// current question, which is a worse query but a working one.
func (s *Service) retrievalQuery(ctx context.Context, q Question) (string, int64) {
	if len(q.History) == 0 {
		return q.Text, 0
	}

	start := time.Now()

	condenseCtx, cancel := context.WithTimeout(ctx, condenseTimeout)
	defer cancel()

	rewritten, err := s.condense(condenseCtx, q)
	elapsed := time.Since(start).Milliseconds()

	if err != nil || rewritten == "" {
		fallback := joinForRetrieval(q)
		s.log.WarnContext(ctx, "follow-up rewrite unavailable, using joined query",
			slog.Int("history_turns", len(q.History)),
			slog.Int64("condense_ms", elapsed),
			slog.Any("error", err))
		return fallback, elapsed
	}

	// Deliberately not logging either question: the request log records shape,
	// never content.
	s.log.DebugContext(ctx, "rewrote follow-up for retrieval",
		slog.Int("history_turns", len(q.History)),
		slog.Bool("changed", rewritten != q.Text),
		slog.Int64("condense_ms", elapsed))

	return rewritten, elapsed
}

// condense asks the model for the standalone form.
func (s *Service) condense(ctx context.Context, q Question) (string, error) {
	var b strings.Builder
	b.WriteString("Conversation so far:\n\n")
	writeHistory(&b, q.History)
	b.WriteString("\nLatest question: ")
	b.WriteString(q.Text)
	b.WriteString("\n\nRewritten standalone question:")

	stream, err := s.llm.Stream(ctx, ai.Request{
		System:    condensePrompt,
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: b.String()}},
		MaxTokens: condenseMaxTokens,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var out strings.Builder
	for stream.Next() {
		if event := stream.Event(); event.Type == ai.EventText {
			out.WriteString(event.Text)
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}

	return cleanRewrite(out.String(), q.Text), nil
}

// cleanRewrite normalises the model's output and rejects anything that does not
// look like a question.
//
// A rewrite that came back as a paragraph, an answer, or an empty string is
// discarded — searching for the model's prose instead of the user's question is
// worse than searching for the original.
func cleanRewrite(raw, original string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}

	// Keep the first line: anything after it is commentary the prompt asked for
	// and did not get.
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	text = strings.Trim(text, `"'“”`)
	text = strings.TrimSpace(text)

	if text == "" {
		return ""
	}
	// Three times the original is not a rewrite of it.
	if limit := 3*len([]rune(original)) + 200; len([]rune(text)) > limit {
		return ""
	}
	return text
}

// joinForRetrieval is the fallback query: the most recent user turn followed by
// the current question, so the terms a pronoun stands in for are at least
// present for lexical and vector search to find.
func joinForRetrieval(q Question) string {
	for i := len(q.History) - 1; i >= 0; i-- {
		if q.History[i].Role != ai.RoleUser {
			continue
		}
		previous := strings.TrimSpace(q.History[i].Text)
		if previous == "" {
			continue
		}
		return previous + "\n" + q.Text
	}
	return q.Text
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

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}
