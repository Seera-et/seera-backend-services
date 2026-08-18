// Package qa is the HTTP surface of AI Legal Q&A: one streaming endpoint.
//
// It owns the wire contract and nothing else. Retrieval, generation and
// grounding live in internal/rag.
package qa

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/rag"
)

// maxQuestionRunes bounds the question. Long enough for a paragraph of context
// about a situation, short enough that nobody pastes a contract in and expects
// document analysis — that is a different feature.
const maxQuestionRunes = 2000

// maxHistoryRunes bounds the whole conversation a client may send. Individual
// turns are trimmed by rag.TrimHistory; this rejects the request outright, so a
// client cannot use the history field as an unbounded upload.
const maxHistoryRunes = 24000

// HistoryTurn is one earlier exchange in the same conversation.
//
// Conversations are not stored server-side, so a follow-up carries the turns it
// depends on. Only "user" and "assistant" are accepted: a client cannot inject a
// system instruction through this field.
type HistoryTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Request is the body of POST /api/v1/qa/query.
type Request struct {
	Question string `json:"question"`

	// Language restricts sources to one language. Empty searches both, which is
	// the default: an English question can be answered by an Amharic provision.
	Language string `json:"language,omitempty"`

	// AsOf asks what the law was on a given date, as YYYY-MM-DD. Empty means
	// today. Provisions not in force on this date are excluded in SQL.
	AsOf string `json:"as_of,omitempty"`

	// History is the conversation this question continues, oldest first. Empty
	// for the first question in a thread.
	History []HistoryTurn `json:"history,omitempty"`
}

// Validate checks the request and converts it into a domain question.
func (r Request) Validate() (rag.Question, error) {
	question := strings.TrimSpace(r.Question)
	if question == "" {
		return rag.Question{}, errors.New("question is required")
	}
	if len([]rune(question)) > maxQuestionRunes {
		return rag.Question{}, fmt.Errorf("question is longer than %d characters", maxQuestionRunes)
	}

	q := rag.Question{Text: question}

	if r.Language != "" {
		lang := legal.Language(r.Language)
		if !lang.Valid() {
			return rag.Question{}, fmt.Errorf("language %q is not one of en, am", r.Language)
		}
		q.Language = lang
	}

	if r.AsOf != "" {
		asOf, err := time.Parse("2006-01-02", r.AsOf)
		if err != nil {
			return rag.Question{}, fmt.Errorf("as_of must be YYYY-MM-DD, got %q", r.AsOf)
		}
		q.AsOf = asOf
	}

	history, err := validateHistory(r.History)
	if err != nil {
		return rag.Question{}, err
	}
	q.History = history

	return q, nil
}

// validateHistory converts and bounds the supplied conversation.
//
// Rejecting an unknown role rather than coercing it is the point: "system" or
// "developer" arriving from a browser is an attempt to rewrite the generation
// contract, and it should fail loudly rather than be silently relabelled.
func validateHistory(turns []HistoryTurn) ([]rag.Turn, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	if len(turns) > rag.MaxHistoryTurns {
		return nil, fmt.Errorf("history is longer than %d turns", rag.MaxHistoryTurns)
	}

	total := 0
	out := make([]rag.Turn, 0, len(turns))

	for i, turn := range turns {
		var role ai.Role
		switch turn.Role {
		case string(ai.RoleUser):
			role = ai.RoleUser
		case string(ai.RoleAssistant):
			role = ai.RoleAssistant
		default:
			return nil, fmt.Errorf("history[%d].role %q is not one of user, assistant", i, turn.Role)
		}

		text := strings.TrimSpace(turn.Text)
		if text == "" {
			// An empty turn carries nothing and would only cost context.
			continue
		}

		total += len([]rune(text))
		if total > maxHistoryRunes {
			return nil, fmt.Errorf("history is longer than %d characters in total", maxHistoryRunes)
		}

		out = append(out, rag.Turn{Role: role, Text: text})
	}

	// Trimmed again in the service, which is where the limit belongs for callers
	// that do not come through this DTO.
	return rag.TrimHistory(out), nil
}

// sourcesPayload is the `sources` event: the citations, sent after retrieval and
// before the first token so the UI can render source cards while the answer
// streams. This is most of the perceived-latency win.
type sourcesPayload struct {
	Citations []rag.Citation `json:"citations"`
}

// tokenPayload is one `token` event.
//
// A JSON object rather than a bare string because SSE data is newline-delimited,
// and answer text contains newlines. Encoding it as JSON is what keeps a
// multi-line paragraph from being read as several events.
type tokenPayload struct {
	Text string `json:"text"`
}

// donePayload is the terminal `done` event.
type donePayload struct {
	// Kind is "legal", "abstention" or "conversation". The client presents the
	// three differently — a reply to "hello" must not arrive wearing the badges
	// of a grounded legal answer — and inferring it from the booleans below is
	// exactly the kind of guessing that gets a UI wrong.
	Kind string `json:"kind"`

	Citations []rag.Citation `json:"citations"`
	Abstained bool           `json:"abstained"`
	Grounded  bool           `json:"grounded"`
	Timings   rag.Timings    `json:"timings_ms"`
	Model     string         `json:"model"`
	Reranker  string         `json:"reranker"`
	RequestID string         `json:"request_id,omitempty"`
}

// errorPayload is the `error` event.
type errorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}
