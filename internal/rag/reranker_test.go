package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/search"
)

func TestPassthroughRerankerTruncatesWithoutReordering(t *testing.T) {
	results := []search.Result{
		{ArticleNo: "1"}, {ArticleNo: "2"}, {ArticleNo: "3"},
	}

	got, err := PassthroughReranker{}.Rerank(context.Background(), "q", results, 2)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].ArticleNo != "1" || got[1].ArticleNo != "2" {
		t.Errorf("passthrough reordered results: %v", got)
	}
}

func TestPassthroughRerankerHandlesShortLists(t *testing.T) {
	results := []search.Result{{ArticleNo: "1"}}

	got, err := PassthroughReranker{}.Rerank(context.Background(), "q", results, 10)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d results, want 1", len(got))
	}
}

func TestCohereRerankerReordersByRelevance(t *testing.T) {
	var gotRequest cohereRerankRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		// Rank the third candidate first.
		_ = json.NewEncoder(w).Encode(cohereRerankResponse{
			Results: []struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			}{
				{Index: 2, RelevanceScore: 0.91},
				{Index: 0, RelevanceScore: 0.42},
			},
		})
	}))
	defer server.Close()

	reranker, err := NewCohereReranker("test-key", WithCohereBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewCohereReranker() = %v", err)
	}

	results := []search.Result{
		{ArticleNo: "1", Text: "irrelevant", DocumentTitle: "Commercial Code"},
		{ArticleNo: "2", Text: "also irrelevant", DocumentTitle: "Commercial Code"},
		{ArticleNo: "45", Text: "A private limited company may be formed", DocumentTitle: "Commercial Code"},
	}

	got, err := reranker.Rerank(context.Background(), "how do I form a company?", results, 2)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].ArticleNo != "45" {
		t.Errorf("top result = article %s, want 45", got[0].ArticleNo)
	}
	if got[0].Score != 0.91 {
		t.Errorf("score = %f, want the reranker's score", got[0].Score)
	}

	// The cross-encoder must see the contextualised form, not a bare span.
	if len(gotRequest.Documents) != 3 {
		t.Fatalf("sent %d documents, want 3", len(gotRequest.Documents))
	}
	if !strings.Contains(gotRequest.Documents[2], "Commercial Code") ||
		!strings.Contains(gotRequest.Documents[2], "Article 45") {
		t.Errorf("document sent without provenance: %q", gotRequest.Documents[2])
	}
	if gotRequest.TopN != 2 {
		t.Errorf("top_n = %d, want 2", gotRequest.TopN)
	}
}

// An out-of-range index would attach a relevance score to the wrong provision —
// a wrong citation with a confident number next to it.
func TestCohereRerankerRejectsOutOfRangeIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":7,"relevance_score":0.9}]}`))
	}))
	defer server.Close()

	reranker, err := NewCohereReranker("test-key", WithCohereBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewCohereReranker() = %v", err)
	}

	_, err = reranker.Rerank(context.Background(), "q", []search.Result{{ArticleNo: "1"}}, 1)
	if err == nil {
		t.Fatal("Rerank() accepted an out-of-range index")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %q, want it to name the problem", err)
	}
}

func TestCohereRerankerSurfacesHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()

	reranker, _ := NewCohereReranker("test-key", WithCohereBaseURL(server.URL))

	_, err := reranker.Rerank(context.Background(), "q", []search.Result{{ArticleNo: "1"}}, 1)
	if err == nil {
		t.Fatal("Rerank() ignored a 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %q, want the status", err)
	}
}

func TestNewCohereRerankerRequiresAPIKey(t *testing.T) {
	if _, err := NewCohereReranker(""); err == nil {
		t.Error("NewCohereReranker() accepted an empty key")
	}
}

func TestCohereRerankerHandlesEmptyCandidates(t *testing.T) {
	reranker, _ := NewCohereReranker("test-key")

	got, err := reranker.Rerank(context.Background(), "q", nil, 6)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d results for no candidates", len(got))
	}
}
