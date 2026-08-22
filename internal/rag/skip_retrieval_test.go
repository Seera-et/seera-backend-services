package rag

import (
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/citations"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// The point of SkipRetrieval: a follow-up the router judged fully answerable
// from what is already cited must not pay for a fresh search.
func TestSkipRetrievalNeverCallsTheRetriever(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "265"),
	}}
	retriever := &stubRetriever{results: testResults(3)}

	svc := NewService(retriever, PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "what are its advantages?",
		CarriedChunks: []legal.ChunkID{"carried-1"},
		SkipRetrieval: true,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if retriever.got.Text != "" {
		t.Errorf("retriever was queried with %q, want no query at all", retriever.got.Text)
	}
	if len(events[0].Citations) != 1 || events[0].Citations[0].ChunkID != "carried-1" {
		t.Error("the carried source did not reach the answer")
	}
}

// A stale or unresolvable carry must not turn into a spurious abstention: it
// degrades to the same real retrieval every request without SkipRetrieval gets.
func TestSkipRetrievalFallsBackWhenCarriedSourcesResolveToNothing(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{}} // stale id, resolves to nothing
	retriever := &stubRetriever{results: testResults(1)}

	svc := NewService(retriever, PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "what are its advantages?",
		CarriedChunks: []legal.ChunkID{"stale-id"},
		SkipRetrieval: true,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if retriever.got.Text == "" {
		t.Error("a stale carry did not fall back to a real retrieval")
	}
	if len(events[0].Citations) == 0 {
		t.Error("the fallback retrieval's results did not reach the answer")
	}
}

// Honesty is preserved either way: if the skip path's fallback also finds
// nothing, the answer still abstains rather than fabricating one.
func TestSkipRetrievalStillAbstainsWhenNothingIsFound(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{}}
	retriever := &stubRetriever{results: nil}

	svc := NewService(retriever, PassthroughReranker{}, &stubLLM{chunks: []string{"unused"}},
		DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "what are its advantages?",
		CarriedChunks: []legal.ChunkID{"stale-id"},
		SkipRetrieval: true,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1]
	if !done.Result.Abstained {
		t.Error("should have abstained when both the carry and the fallback retrieval found nothing")
	}
}

// Multi-hop's whole premise — something is missing — contradicts why the skip
// path exists at all, so it must never run there even when enabled globally.
func TestSkipRetrievalNeverRunsMultiHop(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "265"),
	}}
	cfg := DefaultConfig()
	cfg.MultiHop = true

	svc := NewService(&stubRetriever{results: testResults(3)}, PassthroughReranker{},
		&stubLLM{chunks: []string{"An answer [S1]."}}, cfg, testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "what are its advantages?",
		CarriedChunks: []legal.ChunkID{"carried-1"},
		SkipRetrieval: true,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Timings.Expand != 0 {
		t.Error("multi-hop ran on the skip-retrieval path")
	}
}

// Carried results are always tagged RetrieverCarried, never RetrieverArticle or
// RetrieverHybrid, so a pure-carry turn must never read as High confidence —
// nothing here was actually matched or corroborated this request.
func TestSkipRetrievalConfidenceIsNeverSpuriouslyHigh(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "265"),
		"carried-2": carriedCitation("carried-2", "266"),
	}}

	svc := NewService(&stubRetriever{results: testResults(3)}, PassthroughReranker{},
		&stubLLM{chunks: []string{"An answer [S1, S2]."}}, DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "what are its advantages?",
		CarriedChunks: []legal.ChunkID{"carried-1", "carried-2"},
		SkipRetrieval: true,
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Confidence != nil && done.Result.Confidence.Level == ConfidenceHigh {
		t.Errorf("confidence = %q from carried sources alone, want anything but High", done.Result.Confidence.Level)
	}
}
