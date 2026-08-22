package dialogue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/chat"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/rag"
	"github.com/Seera-et/seera-backend-services/internal/router"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

type stubRouter struct {
	decision router.Decision
	got      router.Input
}

func (s *stubRouter) Route(_ context.Context, in router.Input) router.Decision {
	s.got = in
	return s.decision
}

type stubGeneral struct {
	reply  chat.Reply
	err    error
	calls  atomic.Int32
	gotMsg string
}

func (s *stubGeneral) Reply(_ context.Context, in chat.Input) (chat.Reply, error) {
	s.calls.Add(1)
	s.gotMsg = in.Message
	return s.reply, s.err
}

type stubLegal struct {
	calls atomic.Int32
	got   rag.Question
}

func (s *stubLegal) Answer(_ context.Context, q rag.Question, emit func(rag.Event) error) error {
	s.calls.Add(1)
	s.got = q
	if err := emit(rag.Event{Type: rag.EventSources, Citations: []rag.Citation{}}); err != nil {
		return err
	}
	if err := emit(rag.Event{Type: rag.EventToken, Text: "A grounded answer [S1]."}); err != nil {
		return err
	}
	return emit(rag.Event{Type: rag.EventDone, Result: &rag.Result{Kind: rag.KindLegal}})
}

type stubPrefetch struct {
	results []search.Result
	calls   atomic.Int32
}

func (s *stubPrefetch) Search(_ context.Context, _ search.Query) ([]search.Result, error) {
	s.calls.Add(1)
	return s.results, nil
}

func newService(t *testing.T, r Router, g GeneralEngine, l LegalEngine, opts ...Option) *Service {
	t.Helper()
	return New(r, g, l, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
}

func collect(t *testing.T, s *Service, q rag.Question) []rag.Event {
	t.Helper()
	var events []rag.Event
	if err := s.Answer(context.Background(), q, func(e rag.Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	return events
}

func answerText(events []rag.Event) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == rag.EventToken {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func TestGeneralMessageIsAnsweredWithoutRetrieval(t *testing.T) {
	general := &stubGeneral{reply: chat.Reply{Text: "Hello. I answer questions about Ethiopian law."}}
	legalEngine := &stubLegal{}

	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent: router.IntentGeneral, Depth: router.DepthExplain,
	}}, general, legalEngine)

	events := collect(t, svc, rag.Question{Text: "Hello"})

	if legalEngine.calls.Load() != 0 {
		t.Error("a greeting reached retrieval")
	}
	if !strings.Contains(answerText(events), "Ethiopian law") {
		t.Errorf("unexpected reply: %q", answerText(events))
	}

	done := events[len(events)-1]
	if done.Result.Kind != rag.KindConversation {
		t.Errorf("kind = %q, want %q", done.Result.Kind, rag.KindConversation)
	}
	if done.Result.Grounded || done.Result.Abstained {
		t.Error("a conversational reply must claim neither grounding nor abstention")
	}
}

// The property that makes a model-driven router safe: a misroute costs latency,
// not correctness.
func TestGeneralReplyThatAssertsLawFallsThroughToRetrieval(t *testing.T) {
	general := &stubGeneral{reply: chat.Reply{Deferred: true, DeferReason: "cites an article"}}
	legalEngine := &stubLegal{}

	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent: router.IntentGeneral, Depth: router.DepthExplain,
	}}, general, legalEngine)

	events := collect(t, svc, rag.Question{Text: "How do I register a company?"})

	if legalEngine.calls.Load() != 1 {
		t.Fatal("a withheld general reply did not fall through to the corpus")
	}
	if !strings.Contains(answerText(events), "grounded answer") {
		t.Errorf("the corpus answer did not reach the client: %q", answerText(events))
	}
	if kind := events[len(events)-1].Result.Kind; kind != rag.KindLegal {
		t.Errorf("kind = %q, want %q", kind, rag.KindLegal)
	}
}

func TestGeneralEngineFailureFallsThroughToRetrieval(t *testing.T) {
	general := &stubGeneral{err: errors.New("provider down")}
	legalEngine := &stubLegal{}

	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent: router.IntentGeneral,
	}}, general, legalEngine)

	collect(t, svc, rag.Question{Text: "Hello"})

	if legalEngine.calls.Load() != 1 {
		t.Error("a failed general reply did not fall through")
	}
}

func TestRouteEventPrecedesEverything(t *testing.T) {
	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent:     router.IntentLegal,
		Depth:      router.DepthAnalyse,
		SearchText: "What are the requirements for forming a private limited company?",
	}}, &stubGeneral{}, &stubLegal{})

	events := collect(t, svc, rag.Question{Text: "What about for a PLC?"})

	if events[0].Type != rag.EventRoute {
		t.Fatalf("first event is %q, want %q", events[0].Type, rag.EventRoute)
	}
	route := events[0].Route
	if route.Intent != string(router.IntentLegal) {
		t.Errorf("intent = %q", route.Intent)
	}
	// The rewrite is shown because the reader could not otherwise tell that
	// their question was searched for as something else.
	if !strings.Contains(route.SearchText, "private limited company") {
		t.Errorf("search text = %q, want the rewrite", route.SearchText)
	}
}

func TestRouteEventHidesARewriteThatChangedNothing(t *testing.T) {
	question := "How do I register a company?"
	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent: router.IntentLegal, SearchText: question,
	}}, &stubGeneral{}, &stubLegal{})

	events := collect(t, svc, rag.Question{Text: question})

	if events[0].Route.SearchText != "" {
		t.Errorf("search text = %q, want empty when it equals the question",
			events[0].Route.SearchText)
	}
}

func TestDecisionShapesTheLegalQuestion(t *testing.T) {
	legalEngine := &stubLegal{}
	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent:          router.IntentDiscussion,
		Depth:           router.DepthAnalyse,
		SearchText:      "What does Article 265 require?",
		UsePriorSources: true,
	}}, &stubGeneral{}, legalEngine)

	collect(t, svc, rag.Question{
		Text:          "explain the second one",
		CarriedChunks: []legal.ChunkID{"chunk-1", "chunk-2"},
	})

	if legalEngine.got.SearchText != "What does Article 265 require?" {
		t.Errorf("search text = %q", legalEngine.got.SearchText)
	}
	if legalEngine.got.Depth != rag.DepthAnalyse {
		t.Errorf("depth = %q, want %q", legalEngine.got.Depth, rag.DepthAnalyse)
	}
	if len(legalEngine.got.CarriedChunks) != 2 {
		t.Errorf("carried %d chunks, want 2", len(legalEngine.got.CarriedChunks))
	}
}

func TestCarriedSourcesAreDroppedWhenTheQuestionMovesOn(t *testing.T) {
	legalEngine := &stubLegal{}
	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent:          router.IntentLegal,
		SearchText:      "What is the minimum notice period?",
		UsePriorSources: false,
	}}, &stubGeneral{}, legalEngine)

	collect(t, svc, rag.Question{
		Text:          "What is the minimum notice period?",
		CarriedChunks: []legal.ChunkID{"chunk-1"},
	})

	if len(legalEngine.got.CarriedChunks) != 0 {
		t.Error("carried sources into a question that had moved on")
	}
}

func TestPrefetchIsUsedWhenTheRouterAgrees(t *testing.T) {
	question := "How do I register a company?"
	prefetcher := &stubPrefetch{results: []search.Result{{ChunkID: "chunk-1"}}}
	legalEngine := &stubLegal{}

	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent: router.IntentLegal, SearchText: question,
	}}, &stubGeneral{}, legalEngine, WithPrefetch(prefetcher, 50))

	collect(t, svc, rag.Question{Text: question})

	if prefetcher.calls.Load() != 1 {
		t.Fatalf("speculative retrieval ran %d times, want 1", prefetcher.calls.Load())
	}
	if len(legalEngine.got.Candidates) != 1 {
		t.Error("the legal engine did not receive the speculative candidates")
	}
}

func TestPrefetchIsDiscardedWhenTheQuestionWasRewritten(t *testing.T) {
	prefetcher := &stubPrefetch{results: []search.Result{{ChunkID: "chunk-1"}}}
	legalEngine := &stubLegal{}

	svc := newService(t, &stubRouter{decision: router.Decision{
		Intent:     router.IntentLegal,
		SearchText: "What are the requirements for forming a private limited company?",
	}}, &stubGeneral{}, legalEngine, WithPrefetch(prefetcher, 50))

	collect(t, svc, rag.Question{Text: "and for a PLC?"})

	// Results fetched for the raw text answer a different question.
	if legalEngine.got.Candidates != nil {
		t.Error("speculative candidates were used after a rewrite")
	}
}

func TestPrefetchDoesNotRunOnAFollowUp(t *testing.T) {
	prefetcher := &stubPrefetch{}
	svc := newService(t, &stubRouter{decision: router.Decision{Intent: router.IntentLegal}},
		&stubGeneral{}, &stubLegal{}, WithPrefetch(prefetcher, 50))

	collect(t, svc, rag.Question{
		Text:    "and for a PLC?",
		History: []rag.Turn{{Role: ai.RoleUser, Text: "How do I register a company?"}},
	})

	if prefetcher.calls.Load() != 0 {
		t.Error("speculated on a follow-up, whose rewrite is the whole point")
	}
}

func TestEmptyQuestionIsRejected(t *testing.T) {
	svc := newService(t, &stubRouter{}, &stubGeneral{}, &stubLegal{})

	err := svc.Answer(context.Background(), rag.Question{Text: "  "},
		func(rag.Event) error { return nil })
	if err == nil {
		t.Error("empty question accepted")
	}
}
