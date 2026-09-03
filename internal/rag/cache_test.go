package rag

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// stubAnswerer is a scripted Answerer standing in for rag.Service, so Cache's
// behavior can be tested without a real retriever, reranker, or LLM.
type stubAnswerer struct {
	calls  int
	events []Event
	err    error
}

func (s *stubAnswerer) Answer(_ context.Context, _ Question, emit func(Event) error) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	for _, e := range s.events {
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}

func legalAnswerEvents(citations []Citation, text string) []Event {
	return []Event{
		{Type: EventSources, Citations: citations},
		{Type: EventToken, Text: text},
		{Type: EventDone, Citations: citations, Result: &Result{
			Kind:      KindLegal,
			Grounded:  true,
			Citations: citations,
			Timings:   Timings{CandidateN: len(citations), ContextChunk: len(citations), Rerank: 42, FirstToken: 99, Total: 123},
		}},
	}
}

func newTestCache(inner Answerer, retriever Retriever) *Cache {
	return NewCache(inner, retriever, 50, CacheConfig{Capacity: 10, TTL: time.Minute}, testLogger())
}

func collectCache(t *testing.T, c *Cache, q Question) []Event {
	t.Helper()
	var events []Event
	if err := c.Answer(context.Background(), q, func(e Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatalf("Answer() = %v", err)
	}
	return events
}

func TestCacheServesAHitWithoutCallingInnerAgain(t *testing.T) {
	citations := []Citation{{Marker: "S1", ChunkID: "chunk-1"}}
	inner := &stubAnswerer{events: legalAnswerEvents(citations, "A private limited company needs paid-up capital.")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "What is needed to form a private limited company?"}

	first := collectCache(t, cache, q)
	second := collectCache(t, cache, q)

	if inner.calls != 1 {
		t.Errorf("inner.calls = %d, want 1 — the second identical question should have been served from cache", inner.calls)
	}

	firstText := tokenText(first)
	secondText := tokenText(second)
	if firstText != secondText {
		t.Errorf("cached answer text = %q, want %q", secondText, firstText)
	}
	if len(second) != 3 {
		t.Fatalf("cached replay emitted %d events, want 3 (sources, token, done)", len(second))
	}
	if second[2].Result.Kind != KindLegal {
		t.Errorf("cached result kind = %q, want %q", second[2].Result.Kind, KindLegal)
	}
}

func TestCacheMissesWhenRetrievedChunksChange(t *testing.T) {
	citations := []Citation{{Marker: "S1", ChunkID: "chunk-1"}}
	inner := &stubAnswerer{events: legalAnswerEvents(citations, "answer")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "What is needed to form a private limited company?"}
	collectCache(t, cache, q)

	// A newer version of the corpus now returns a different chunk set for the
	// same question — the exact scenario the chunk-ID check exists to catch.
	retriever.results = testResults(4)
	collectCache(t, cache, q)

	if inner.calls != 2 {
		t.Errorf("inner.calls = %d, want 2 — a changed retrieval result must not be served from the old cache entry", inner.calls)
	}
}

func TestCacheSkipsCarriedSources(t *testing.T) {
	inner := &stubAnswerer{events: legalAnswerEvents(nil, "answer")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "what about the second one", CarriedChunks: []legal.ChunkID{"chunk-1"}}
	collectCache(t, cache, q)
	collectCache(t, cache, q)

	if inner.calls != 2 {
		t.Errorf("inner.calls = %d, want 2 — a question carrying earlier sources must never be cached", inner.calls)
	}
	if retriever.got.Text != "" {
		t.Error("retriever was called for an uncacheable question — the freshness check should never have run")
	}
}

func TestCacheSkipsQuestionsWithHistory(t *testing.T) {
	inner := &stubAnswerer{events: legalAnswerEvents(nil, "answer")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{
		Text:    "What is needed to form a private limited company?",
		History: []Turn{{Role: ai.RoleUser, Text: "earlier turn"}},
	}
	collectCache(t, cache, q)
	collectCache(t, cache, q)

	if inner.calls != 2 {
		t.Error("a question with conversation history must never be cached — generation reads history verbatim")
	}
}

func TestCacheSkipsSkipRetrieval(t *testing.T) {
	inner := &stubAnswerer{events: legalAnswerEvents(nil, "answer")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "is that still in force", SkipRetrieval: true}
	collectCache(t, cache, q)
	collectCache(t, cache, q)

	if inner.calls != 2 {
		t.Error("a SkipRetrieval question must never be cached")
	}
}

func TestCacheDoesNotCacheAbstentions(t *testing.T) {
	inner := &stubAnswerer{events: []Event{
		{Type: EventSources, Citations: []Citation{}},
		{Type: EventToken, Text: AbstainMessage},
		{Type: EventDone, Citations: []Citation{}, Result: &Result{Kind: KindAbstention, Abstained: true, Grounded: true}},
	}}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "something entirely outside the corpus"}
	collectCache(t, cache, q)
	collectCache(t, cache, q)

	if inner.calls != 2 {
		t.Error("an abstention reached no generation and should not be cached")
	}
}

func TestCacheKeyDistinguishesDepth(t *testing.T) {
	citations := []Citation{{Marker: "S1", ChunkID: "chunk-1"}}
	inner := &stubAnswerer{events: legalAnswerEvents(citations, "answer")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "What does Article 45 say?"}
	q.Depth = DepthQuote
	collectCache(t, cache, q)

	q.Depth = DepthAnalyse
	collectCache(t, cache, q)

	if inner.calls != 2 {
		t.Error("the same question text at a different depth must not share a cache entry — depth changes both topN and the generation instructions")
	}
}

func TestCacheFallsBackWhenRetrievalFails(t *testing.T) {
	inner := &stubAnswerer{events: legalAnswerEvents(nil, "answer")}
	retriever := &stubRetriever{err: errors.New("db down")}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "What is needed to form a private limited company?"}
	collectCache(t, cache, q)

	if inner.calls != 1 {
		t.Error("a retrieval failure during the freshness check must fail open to a normal answer, not fail the request")
	}
}

func TestCacheReusesSuppliedCandidatesInsteadOfSearchingAgain(t *testing.T) {
	citations := []Citation{{Marker: "S1", ChunkID: "chunk-1"}}
	inner := &stubAnswerer{events: legalAnswerEvents(citations, "answer")}
	retriever := &stubRetriever{results: testResults(3)}
	cache := newTestCache(inner, retriever)

	q := Question{Text: "What is needed to form a private limited company?", Candidates: testResults(2)}
	collectCache(t, cache, q)

	if retriever.got.Text != "" {
		t.Error("retriever.Search was called even though the question already carried prefetched candidates")
	}
}

func tokenText(events []Event) string {
	var out string
	for _, e := range events {
		if e.Type == EventToken {
			out += e.Text
		}
	}
	return out
}

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := newLRU(2, time.Minute)
	a, b, d := cacheKey{text: "a"}, cacheKey{text: "b"}, cacheKey{text: "d"}

	c.set(a, cacheEntry{answerText: "a"})
	c.set(b, cacheEntry{answerText: "b"})
	if _, ok := c.get(a); !ok {
		t.Fatal("a should still be cached")
	}
	// a was just touched by get(), so it's more recently used than b; adding a
	// third entry should evict b, not a.
	c.set(d, cacheEntry{answerText: "d"})

	if _, ok := c.get(b); ok {
		t.Error("b should have been evicted as the least recently used entry")
	}
	if _, ok := c.get(a); !ok {
		t.Error("a should not have been evicted")
	}
	if _, ok := c.get(d); !ok {
		t.Error("d should be cached")
	}
}

func TestLRUExpiresEntriesAfterTTL(t *testing.T) {
	c := newLRU(10, -time.Second) // already expired the instant it's set
	key := cacheKey{text: "a"}
	c.set(key, cacheEntry{answerText: "a"})

	if _, ok := c.get(key); ok {
		t.Error("an expired entry was served")
	}
}

func TestCacheKeyForNormalizesQuestionText(t *testing.T) {
	a := cacheKeyFor(Question{Text: "  What   is  needed?  "})
	b := cacheKeyFor(Question{Text: "what is needed?"})
	if a != b {
		t.Errorf("keys differ for the same question modulo whitespace/case: %+v vs %+v", a, b)
	}
}

func TestCacheKeyForTreatsZeroAsOfAsToday(t *testing.T) {
	a := cacheKeyFor(Question{Text: "q"})
	b := cacheKeyFor(Question{Text: "q", AsOf: time.Time{}})
	if a != b {
		t.Error("a zero AsOf should key the same as another zero AsOf")
	}
}

func TestEqualChunkIDsIgnoresOrder(t *testing.T) {
	a := []legal.ChunkID{"x", "y"}
	b := []legal.ChunkID{"y", "x"}
	if equalChunkIDs(a, b) {
		t.Error("equalChunkIDs should compare pre-sorted input; unsorted equal-by-set slices must not be reported equal")
	}
	sortedA := sortedChunkIDs([]search.Result{{ChunkID: "x"}, {ChunkID: "y"}})
	sortedB := sortedChunkIDs([]search.Result{{ChunkID: "y"}, {ChunkID: "x"}})
	if !equalChunkIDs(sortedA, sortedB) {
		t.Error("sortedChunkIDs should make retrieval order irrelevant to the freshness check")
	}
}
