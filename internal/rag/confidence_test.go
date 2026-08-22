package rag

import (
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/search"
)

func TestConfidenceFromNoResults(t *testing.T) {
	got := confidenceFrom(nil, 0, 0)
	if got.Level != ConfidenceLow {
		t.Errorf("level = %q, want %q", got.Level, ConfidenceLow)
	}
}

func TestConfidenceFromExactMatchIsHigh(t *testing.T) {
	top := []search.Result{
		{Retriever: search.RetrieverArticle, Score: 0.5},
	}
	got := confidenceFrom(top, 1, 0)

	if got.Level != ConfidenceHigh {
		t.Errorf("level = %q, want %q", got.Level, ConfidenceHigh)
	}
	if !got.ExactMatch {
		t.Error("ExactMatch = false, want true for an article-lookup result")
	}
}

func TestConfidenceFromMultipleRetrieversAgreeingIsHigh(t *testing.T) {
	top := []search.Result{
		{Retriever: search.RetrieverHybrid, Score: 0.9},
		{Retriever: search.RetrieverVector, Score: 0.7},
	}
	got := confidenceFrom(top, 2, 0)

	if got.Level != ConfidenceHigh {
		t.Errorf("level = %q, want %q", got.Level, ConfidenceHigh)
	}
	if got.ExactMatch {
		t.Error("ExactMatch = true, want false — nothing here came from article lookup")
	}
}

// With the passthrough reranker (MinRerankScore == 0, the setup every other
// test in this package already uses), a single uncorroborated result is Low —
// there is nothing here to be confident about beyond "something matched."
func TestConfidenceFromSingleWeakResultIsLow(t *testing.T) {
	top := []search.Result{{Retriever: search.RetrieverVector, Score: 12.5}}
	got := confidenceFrom(top, 1, 0)

	if got.Level != ConfidenceLow {
		t.Errorf("level = %q, want %q", got.Level, ConfidenceLow)
	}
}

func TestConfidenceFromOrdinaryResultsIsMedium(t *testing.T) {
	top := []search.Result{
		{Retriever: search.RetrieverVector, Score: 0.4},
		{Retriever: search.RetrieverKeyword, Score: 0.3},
	}
	got := confidenceFrom(top, 2, 0)

	if got.Level != ConfidenceMedium {
		t.Errorf("level = %q, want %q", got.Level, ConfidenceMedium)
	}
}

func TestConfidenceFromCalibratedStrongScoreIsHigh(t *testing.T) {
	top := []search.Result{{Retriever: search.RetrieverVector, Score: 0.9}}
	got := confidenceFrom(top, 1, 0.5) // minRerankScore=0.5, score well above 1.5x it

	if got.Level != ConfidenceHigh {
		t.Errorf("level = %q, want %q", got.Level, ConfidenceHigh)
	}
}

func TestConfidenceSourceCountIsReportedAsGiven(t *testing.T) {
	top := []search.Result{{Retriever: search.RetrieverVector, Score: 0.4}}
	got := confidenceFrom(top, 3, 0)

	if got.SourceCount != 3 {
		t.Errorf("SourceCount = %d, want 3 (the built context count, not len(top))", got.SourceCount)
	}
}
