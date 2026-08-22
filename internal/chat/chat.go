// Package chat answers the messages that are not questions about Ethiopian law.
//
// Greetings, what Seera can do, requests to rephrase, and general concepts
// explained as general knowledge. It has no corpus, no retrieval and no
// citations, and that is the point: everything it says comes from the model's
// own memory, so it is built so that nothing it says can be a statement about
// Ethiopian law.
//
// Three layers enforce that, in order of reliability:
//
//  1. It is never given sources, so it has nothing to cite and cannot pretend to.
//  2. Its prompt forbids asserting Ethiopian law and requires general concepts to
//     be labelled as general knowledge.
//  3. Every reply is checked before anyone sees it. A reply that reads like a
//     legal assertion is discarded, and the caller sends the message to
//     retrieval instead.
//
// The third layer is why a routing mistake is survivable. The router can be
// wrong; this package cannot be allowed to turn that into an uncited claim about
// the law.
package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

const (
	// Timeout bounds the reply. A general answer that takes longer than this is
	// not worth the wait when a real question may be behind it.
	Timeout = 12 * time.Second

	// MaxOutputTokens keeps replies short. This engine explains a concept or
	// says what Seera does; it does not write essays.
	MaxOutputTokens = 600

	maxHistoryTurns = 6
	maxTurnRunes    = 700
)

// Turn is one earlier exchange.
type Turn struct {
	Role ai.Role
	Text string
}

// Input is one general-reply request.
type Input struct {
	Message string
	History []Turn

	// Category refines what kind of non-legal message this is, from the
	// router's classification. The zero value gets the base prompt with no
	// category-specific guidance.
	Category Category
}

// Reply is what the engine produced.
type Reply struct {
	Text string

	// Deferred is set when the reply was withheld because it read as a statement
	// about Ethiopian law. The caller must answer the message from the corpus
	// instead; the text is empty and must not be shown.
	Deferred bool

	// DeferReason is for the log, never for the user.
	DeferReason string
}

// Engine answers general messages.
type Engine struct {
	llm ai.LLM
	log *slog.Logger
}

func New(llm ai.LLM, log *slog.Logger) *Engine {
	return &Engine{llm: llm, log: log}
}

// Reply answers a message that the router judged not to be a legal question.
//
// Returns Deferred when the answer must come from the corpus after all — either
// because the model tried to state law, or because it said so itself.
func (e *Engine) Reply(ctx context.Context, in Input) (Reply, error) {
	message := strings.TrimSpace(in.Message)
	if message == "" {
		return Reply{}, fmt.Errorf("chat: message is empty")
	}

	// Canned rather than generated: nothing about an out-of-scope message
	// needs a model, and a canned reply cannot be talked into answering it
	// anyway, unlike an instruction a model could in principle be argued out of.
	if in.Category == CategoryOutOfScope {
		return Reply{Text: OutOfScopeMessage}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	start := time.Now()
	raw, _, err := e.llm.Complete(ctx, ai.Request{
		System:    systemPromptFor(in.Category),
		Messages:  []ai.Message{{Role: ai.RoleUser, Content: buildPrompt(in)}},
		MaxTokens: MaxOutputTokens,
	})
	if err != nil {
		return Reply{}, fmt.Errorf("chat: generate: %w", err)
	}

	text := strings.TrimSpace(raw)
	if text == "" {
		return Reply{}, fmt.Errorf("chat: empty reply")
	}

	if reason, asserted := AssertsLaw(text); asserted {
		// The reply is discarded unread. Not shown, not logged in full — a
		// statement about Ethiopian law with nothing behind it is exactly what
		// must never reach a user.
		e.log.InfoContext(ctx, "general reply withheld, deferring to retrieval",
			slog.String("reason", reason),
			slog.Duration("elapsed", time.Since(start)))
		return Reply{Deferred: true, DeferReason: reason}, nil
	}

	e.log.DebugContext(ctx, "answered generally",
		slog.Duration("elapsed", time.Since(start)))

	return Reply{Text: text}, nil
}
