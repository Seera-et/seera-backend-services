// Package dialogue is the branch point of the answer path.
//
// One message arrives. The router says what it is; this decides who answers it,
// and turns whatever they produce into the one event stream the HTTP layer
// knows how to send.
//
// It holds the rules that only make sense above a single engine:
//
//   - Retrieval starts before the router has finished, on the turns where the
//     question is almost certainly the search query already.
//   - A general reply that reads like a statement about Ethiopian law is
//     discarded and the message is answered from the corpus instead. This is
//     what makes a routing mistake survivable.
//   - Anything unexpected resolves to the corpus.
//
// It contains no prompt and no SQL. Engines answer; this one only chooses.
package dialogue

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/chat"
	"github.com/Seera-et/seera-backend-services/internal/citations"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/rag"
	"github.com/Seera-et/seera-backend-services/internal/router"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// Router decides what a message is asking for.
type Router interface {
	Route(ctx context.Context, in router.Input) router.Decision
}

// GeneralEngine answers messages that are not questions about the law.
type GeneralEngine interface {
	Reply(ctx context.Context, in chat.Input) (chat.Reply, error)
}

// LegalEngine answers from the corpus.
type LegalEngine interface {
	Answer(ctx context.Context, q rag.Question, emit func(rag.Event) error) error
}

// Prefetcher is the retrieval used speculatively, while the router is deciding.
type Prefetcher interface {
	Search(ctx context.Context, q search.Query) ([]search.Result, error)
}

// SourceLabeller names the provisions a conversation has already cited, so the
// router can recognise "the second one".
//
// Server-side on purpose. The client knows these labels — it is displaying them
// — but text from a client that reaches a prompt is an injection surface, and
// this one is cheap to read from the database instead.
type SourceLabeller interface {
	ResolveSome(ctx context.Context, ids []legal.ChunkID) ([]citations.Citation, error)
}

// Service dispatches one message to whichever engine should answer it.
type Service struct {
	router   Router
	general  GeneralEngine
	legal    LegalEngine
	prefetch Prefetcher
	sources  SourceLabeller

	// candidates mirrors rag.Config.Candidates, so a speculative retrieval
	// returns the same set the legal engine would have fetched itself.
	candidates int

	log *slog.Logger
}

// Option configures optional collaborators.
type Option func(*Service)

// WithPrefetch enables speculative retrieval: on the first turn of a
// conversation, retrieval starts in parallel with routing.
//
// Worth it because that turn's question is nearly always the search query
// already, so the router's latency is spent on work that would have happened
// anyway. It costs one wasted embedding when the message turns out not to be a
// legal question at all.
func WithPrefetch(p Prefetcher, candidates int) Option {
	return func(s *Service) {
		s.prefetch = p
		if candidates > 0 {
			s.candidates = candidates
		}
	}
}

// WithSourceLabels lets the router see which provisions are already on screen.
func WithSourceLabels(l SourceLabeller) Option {
	return func(s *Service) { s.sources = l }
}

func New(r Router, general GeneralEngine, legal LegalEngine, log *slog.Logger, opts ...Option) *Service {
	s := &Service{
		router:     r,
		general:    general,
		legal:      legal,
		candidates: 50,
		log:        log,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Answer implements the interface the HTTP handler depends on.
func (s *Service) Answer(ctx context.Context, q rag.Question, emit func(rag.Event) error) error {
	if strings.TrimSpace(q.Text) == "" {
		return fmt.Errorf("question is empty")
	}

	// Speculative retrieval, started before the routing decision exists.
	pending := s.startPrefetch(ctx, q)

	decision := s.router.Route(ctx, router.Input{
		Question:     q.Text,
		History:      toRouterTurns(q.History),
		PriorSources: s.priorSourceLabels(ctx, q.CarriedChunks),
	})

	if err := emit(rag.Event{Type: rag.EventRoute, Route: &rag.Route{
		Intent:         routeLabel(decision),
		Depth:          rag.Depth(decision.Depth),
		SearchText:     rewriteToShow(q.Text, decision.SearchText),
		CarriedSources: carriedCount(decision, q),
	}}); err != nil {
		return err
	}

	if decision.Intent == router.IntentGeneral {
		answered, err := s.answerGenerally(ctx, q, decision, emit)
		if err != nil {
			return err
		}
		if answered {
			return nil
		}
		// The general engine declined: the message needs the corpus after all.
		s.log.InfoContext(ctx, "general reply deferred to retrieval")
	}

	return s.legal.Answer(ctx, s.legalQuestion(q, decision, pending), emit)
}

// legalQuestion applies the routing decision to the question the legal engine
// receives.
func (s *Service) legalQuestion(
	q rag.Question,
	decision router.Decision,
	pending *prefetch,
) rag.Question {
	q.SearchText = decision.SearchText
	q.Depth = rag.Depth(decision.Depth)

	if !decision.UsePriorSources {
		// The reader is asking something new; carrying the old provisions would
		// bias retrieval towards what they have already read.
		q.CarriedChunks = nil
	}

	// Re-checked here, not just read off the decision: CarriedChunks may have
	// just been zeroed above, or the client may simply have sent none this turn.
	q.SkipRetrieval = decision.SourcesSufficient && len(q.CarriedChunks) > 0

	q.Candidates = pending.results(q.Text, decision)
	return q
}

// answerGenerally runs the general engine and streams its reply.
//
// Returns false when the reply was withheld, which means the caller must answer
// from the corpus instead. The reply is generated in full before any of it is
// sent, because a reply that may have to be discarded cannot be streamed.
func (s *Service) answerGenerally(
	ctx context.Context,
	q rag.Question,
	decision router.Decision,
	emit func(rag.Event) error,
) (bool, error) {
	start := time.Now()

	reply, err := s.general.Reply(ctx, chat.Input{
		Message:  q.Text,
		History:  toChatTurns(q.History),
		Category: chat.Category(decision.Category),
	})
	if err != nil {
		// The general path failing is not a reason to fail the message: the
		// corpus can still answer it, or say that it cannot.
		s.log.WarnContext(ctx, "general engine failed, falling back to retrieval",
			slog.Any("error", err))
		return false, nil
	}
	if reply.Deferred {
		return false, nil
	}

	// A conversational reply has no sources, and says so by sending an empty
	// list rather than by omitting the event: the client's state machine is the
	// same for every answer.
	if err := emit(rag.Event{Type: rag.EventSources, Citations: []rag.Citation{}}); err != nil {
		return true, err
	}
	if err := emit(rag.Event{Type: rag.EventToken, Text: reply.Text}); err != nil {
		return true, err
	}

	return true, emit(rag.Event{
		Type:      rag.EventDone,
		Citations: []rag.Citation{},
		Result: &rag.Result{
			Kind:   rag.KindConversation,
			Intent: routeLabel(decision),
			// Neither applies: nothing was searched, and nothing was claimed.
			// Both false so that a client reading only the booleans cannot
			// present this as a grounded legal answer.
			Abstained: false,
			Grounded:  false,
			Citations: []rag.Citation{},
			Timings:   rag.Timings{Total: time.Since(start).Milliseconds()},
		},
	})
}

// routeLabel is what the client sees in the open "intent" string field: the
// fine-grained category when the router resolved one, the coarse intent
// otherwise. No frontend schema change is needed for this — that field is
// validated only as a string, not a closed enum.
func routeLabel(decision router.Decision) string {
	if decision.Category != "" {
		return string(decision.Category)
	}
	return string(decision.Intent)
}

// rewriteToShow returns the search text only when it differs from what the
// reader typed. Echoing their own question back adds nothing.
func rewriteToShow(question, searchText string) string {
	if searchText == "" || strings.TrimSpace(searchText) == strings.TrimSpace(question) {
		return ""
	}
	return searchText
}

func carriedCount(decision router.Decision, q rag.Question) int {
	if !decision.UsePriorSources {
		return 0
	}
	return len(q.CarriedChunks)
}

func toRouterTurns(history []rag.Turn) []router.Turn {
	out := make([]router.Turn, 0, len(history))
	for _, turn := range history {
		out = append(out, router.Turn{Role: turn.Role, Text: turn.Text})
	}
	return out
}

func toChatTurns(history []rag.Turn) []chat.Turn {
	out := make([]chat.Turn, 0, len(history))
	for _, turn := range history {
		out = append(out, chat.Turn{Role: turn.Role, Text: turn.Text})
	}
	return out
}

// priorSourceLabels names the carried provisions for the router prompt.
//
// Best effort: without labels the router still classifies, it just has less to
// resolve "the second one" against.
func (s *Service) priorSourceLabels(ctx context.Context, ids []legal.ChunkID) []string {
	if len(ids) == 0 || s.sources == nil {
		return nil
	}

	resolved, err := s.sources.ResolveSome(ctx, ids)
	if err != nil {
		s.log.DebugContext(ctx, "could not label earlier sources", slog.Any("error", err))
		return nil
	}

	labels := make([]string, 0, len(resolved))
	for _, c := range resolved {
		label := c.DocumentTitle
		if c.ArticleNo != "" {
			label += ", Article " + c.ArticleNo
		}
		labels = append(labels, label)
	}
	return labels
}
