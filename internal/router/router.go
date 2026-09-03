// Package router decides what a message is asking for, before anything is
// retrieved.
//
// It replaces a phrase list that could only recognise the greetings someone had
// thought to write down. A model reads the message and the conversation it sits
// in, and returns a decision: which engine should answer, and what retrieval
// should search for if the answer needs the corpus.
//
// Two properties matter more than accuracy:
//
//   - It fails towards the law. A timeout, a malformed response, a provider
//     outage — all resolve to IntentLegal, which is the expensive path and the
//     safe one.
//   - It is advice, not authority. Nothing here can make a claim about Ethiopian
//     law reach a user. The engine on the other side of a Legal decision still
//     validates every citation, and the engine on the other side of a General
//     decision is structurally unable to state law at all.
//
// One call does classification and the follow-up rewrite together. A follow-up
// already paid for a rewrite; splitting the two would put a second model call in
// front of retrieval and spend the time-to-first-token budget on plumbing.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/lang"
)

// Intent is which engine should answer.
type Intent string

const (
	// IntentLegal is a question about Ethiopian law. Goes to retrieval.
	IntentLegal Intent = "legal"

	// IntentDiscussion is a question about sources already cited in this
	// conversation — "explain the second one", "what does that mean in
	// practice". Goes to retrieval too, but carrying those sources.
	IntentDiscussion Intent = "discussion"

	// IntentGeneral is anything else a legal assistant should still answer:
	// greetings, what Seera can do, and general concepts explained as general
	// knowledge rather than as Ethiopian law.
	IntentGeneral Intent = "general"
)

// Category refines an IntentGeneral decision into what kind of non-legal
// message this is, so the general engine can vary its tone. Legal and
// Discussion decisions always carry the zero value — the engine on that side
// has no use for it.
type Category string

const (
	// CategoryGreeting is a greeting, thanks, farewell, or pure pleasantry
	// with no question in it.
	CategoryGreeting Category = "greeting"

	// CategoryGeneralKnowledge is a concept question that does not depend on
	// Ethiopian law in particular, or a request for a service Seera does not
	// provide (drafting, document review, advice on the reader's own
	// situation).
	CategoryGeneralKnowledge Category = "general_knowledge"

	// CategoryProductQuestion asks what Seera is, what it can do, or how it
	// works.
	CategoryProductQuestion Category = "product_question"

	// CategoryOutOfScope is unrelated to Ethiopian law and to Seera itself.
	CategoryOutOfScope Category = "out_of_scope"

	// CategoryClarificationNeeded depends on an antecedent nothing earlier in
	// the conversation resolves — "how do I register it" with no "it" to
	// point at.
	CategoryClarificationNeeded Category = "clarification_needed"
)

// Depth is how much the answer should do with what it retrieves.
type Depth string

const (
	// DepthQuote — the reader wants the provision. "What does Article 265 say?"
	DepthQuote Depth = "quote"

	// DepthExplain — the reader wants to understand it. "What does that mean?"
	DepthExplain Depth = "explain"

	// DepthAnalyse — the reader wants several provisions applied together.
	// "How do these fit? What applies if I have a foreign partner?"
	DepthAnalyse Depth = "analyse"
)

// Decision is what the router concluded.
type Decision struct {
	Intent Intent
	Depth  Depth

	// Category refines Intent == IntentGeneral into what kind of non-legal
	// message this is. Empty for Legal and Discussion.
	Category Category

	// SearchText is what retrieval should search for: the question itself when
	// it stands alone, otherwise a rewrite that resolves the references in it.
	SearchText string

	// UsePriorSources asks the answer path to carry the citations already in
	// this conversation into the context.
	UsePriorSources bool

	// SourcesSufficient narrows UsePriorSources further: true means the answer
	// path can skip a fresh search entirely and rely only on what is already
	// cited. Only ever true for IntentDiscussion. False is always safe — it
	// just means a search runs that might not have been needed.
	SourcesSufficient bool

	// Reason is one short clause, for the log. Never shown to a user.
	Reason string

	// Fallback records that the model did not decide this — a timeout, an error,
	// or a response that failed validation — and the safe default was used.
	Fallback bool
}

// Turn is one earlier exchange. Mirrors the shape the answer path already uses.
type Turn struct {
	Role ai.Role
	Text string
}

// Input is one routing request.
type Input struct {
	Question string
	History  []Turn

	// PriorSources describes the citations currently on the reader's screen, as
	// short labels ("Commercial Code, Article 265"). The model needs to know
	// they exist to recognise "the second one"; it never needs their text.
	PriorSources []string
}

// Bounds. The router sits in front of retrieval, so its whole job has to fit in
// a fraction of the time-to-first-token budget.
const (
	// Timeout is generous enough for a small structured response and short
	// enough that a slow provider cannot hold up the answer. On expiry the
	// caller gets the legal fallback.
	Timeout = 4 * time.Second

	// MaxOutputTokens caps the decision. It is one object with a rewritten
	// sentence in it.
	MaxOutputTokens = 200

	// maxHistoryTurns and maxTurnRunes bound what is quoted back to the model.
	//
	// maxTurnRunes truncates from the front, keeping the start of a turn and
	// dropping its end. Raised from 700, 2026-08-31: found live, a follow-up
	// referencing a lettered section near the end of a long structured answer
	// ("under B") could not be resolved, because the router never saw "B" at
	// all — it sat well past 700 runes into the prior assistant turn. 700 was
	// sized for the shorter, less structured answers this system produced
	// before rule 13 in grounding.go started asking for richer, multi-section
	// answers; it was never revisited when that changed. 4000 comfortably fits
	// a realistic structured answer (this one was ~3,800 runes) without
	// meaningfully growing the router's prompt against context windows in the
	// hundred-thousand-token range. Kept equal to rag.MaxTurnRunes so
	// classification and generation see the same amount of prior content —
	// this bug specifically was routing not seeing what generation would
	// have.
	maxHistoryTurns = 6
	maxTurnRunes    = 4000

	// maxPriorSources bounds the source labels listed for reference.
	maxPriorSources = 10
)

// Router classifies messages.
type Router struct {
	llm ai.LLM
	log *slog.Logger
}

func New(llm ai.LLM, log *slog.Logger) *Router {
	return &Router{llm: llm, log: log}
}

// Route decides how to answer a message.
//
// It never returns an error. Every failure resolves to the legal path with
// Fallback set, because a router that can refuse to answer is a router that can
// take the whole feature down.
func (r *Router) Route(ctx context.Context, in Input) Decision {
	question := strings.TrimSpace(in.Question)
	if question == "" {
		return fallback(in, "empty question")
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	// Classification runs on English cues throughout systemPrompt's category
	// descriptions ("sports", "chit-chat", "registration", "licensing"...).
	// Found live, 2026-08-28: an Amharic question about forming a private
	// limited company — a real "legal" case — was classified "general" /
	// "out_of_scope" by the model, evidently because it didn't weigh Ethiopic
	// script against those English examples the way it weighs English text.
	// Translating just for classification fixes that without touching
	// SearchText's own language handling below, which already resolves
	// through search.Service's own translation (WithTranslation) once
	// retrieval runs. Best-effort: a translation failure classifies the
	// original text, exactly what happened before this existed.
	classifyIn := in
	if lang.HasEthiopicScript(question) {
		translated, err := lang.Translate(ctx, r.llm, question)
		switch {
		case err != nil:
			r.log.WarnContext(ctx, "cross-lingual routing translation failed, classifying the original text",
				slog.Any("error", err))
		case translated != "":
			classifyIn.Question = translated
		}
	}

	start := time.Now()
	raw, _, err := r.llm.Complete(ctx, ai.Request{
		System:     systemPrompt,
		Messages:   []ai.Message{{Role: ai.RoleUser, Content: buildPrompt(classifyIn)}},
		MaxTokens:  MaxOutputTokens,
		JSONSchema: decisionSchema,
	})
	if err != nil {
		r.log.WarnContext(ctx, "router unavailable, treating as a legal question",
			slog.Duration("elapsed", time.Since(start)),
			slog.Any("error", err))
		return fallback(in, "router unavailable")
	}

	decision, err := parseDecision(raw, question, in)
	if err != nil {
		r.log.WarnContext(ctx, "router returned an unusable decision",
			slog.Duration("elapsed", time.Since(start)),
			slog.Any("error", err))
		return fallback(in, "unusable decision")
	}

	// Deliberately not logging the question or the rewrite: the request log
	// records shape, never content.
	r.log.DebugContext(ctx, "routed",
		slog.String("intent", string(decision.Intent)),
		slog.String("depth", string(decision.Depth)),
		slog.Bool("rewritten", decision.SearchText != question),
		slog.Bool("prior_sources", decision.UsePriorSources),
		slog.Duration("elapsed", time.Since(start)))

	return decision
}

// fallback is what happens whenever the model does not give a usable answer:
// the question goes to retrieval, carrying whatever sources are already in the
// conversation, searched for with the best query that can be built without a
// model.
func fallback(in Input, reason string) Decision {
	return Decision{
		Intent:          IntentLegal,
		Depth:           DepthExplain,
		SearchText:      JoinForRetrieval(in),
		UsePriorSources: len(in.PriorSources) > 0,
		Reason:          reason,
		Fallback:        true,
	}
}

// parseDecision validates the model's response into a Decision.
//
// Anything unrecognised is an error rather than a coerced value: a decision this
// code does not understand must reach the fallback, not be guessed at.
func parseDecision(raw, question string, in Input) (Decision, error) {
	var payload struct {
		Intent            string `json:"intent"`
		Depth             string `json:"depth"`
		Category          string `json:"category"`
		SearchText        string `json:"search_text"`
		UsePriorSources   bool   `json:"use_prior_sources"`
		SourcesSufficient bool   `json:"sources_sufficient"`
		Reason            string `json:"reason"`
	}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Decision{}, fmt.Errorf("empty response")
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return Decision{}, fmt.Errorf("decode decision: %w", err)
	}

	intent := Intent(strings.ToLower(strings.TrimSpace(payload.Intent)))
	switch intent {
	case IntentLegal, IntentDiscussion, IntentGeneral:
	default:
		return Decision{}, fmt.Errorf("unknown intent %q", payload.Intent)
	}

	depth := Depth(strings.ToLower(strings.TrimSpace(payload.Depth)))
	switch depth {
	case DepthQuote, DepthExplain, DepthAnalyse:
	case "":
		depth = DepthExplain
	default:
		return Decision{}, fmt.Errorf("unknown depth %q", payload.Depth)
	}

	decision := Decision{
		Intent:          intent,
		Depth:           depth,
		UsePriorSources: payload.UsePriorSources && len(in.PriorSources) > 0,
		Reason:          truncate(strings.TrimSpace(payload.Reason), 120),
	}

	// A general reply searches for nothing.
	if intent == IntentGeneral {
		decision.Category = parseCategory(payload.Category)
		return decision, nil
	}

	decision.SearchText = CleanRewrite(payload.SearchText, question)
	if decision.SearchText == "" {
		// The classification is still usable; only the rewrite is not.
		decision.SearchText = JoinForRetrieval(in)
	}

	// A question about earlier sources needs them whether or not the model
	// remembered to ask: that is what makes it a discussion. SourcesSufficient
	// can never be true without them either — left false (its zero value) for
	// every other intent and for a sourceless discussion.
	if intent == IntentDiscussion && len(in.PriorSources) > 0 {
		decision.UsePriorSources = true
		decision.SourcesSufficient = payload.SourcesSufficient
	}

	return decision, nil
}

// parseCategory validates a category string, degrading to the zero value
// rather than failing the whole decision. Unlike Intent and Depth, an
// unrecognized category does not send the message to fallback(): a category
// only ever changes tone inside an already-guarded, non-legal reply
// (guard.AssertsLaw still runs regardless), so a malformed tone label
// shouldn't force a full legal retrieval that would most likely just abstain.
func parseCategory(raw string) Category {
	switch c := Category(strings.ToLower(strings.TrimSpace(raw))); c {
	case CategoryGreeting, CategoryGeneralKnowledge, CategoryProductQuestion,
		CategoryOutOfScope, CategoryClarificationNeeded:
		return c
	default:
		return ""
	}
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
