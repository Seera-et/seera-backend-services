package rag

import (
	"context"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

func llmRerankResults() []search.Result {
	return []search.Result{
		{ArticleNo: "1", Text: "irrelevant", DocumentTitle: "Commercial Code", ChunkID: legal.ChunkID("c1")},
		{ArticleNo: "2", Text: "also irrelevant", DocumentTitle: "Commercial Code", ChunkID: legal.ChunkID("c2")},
		{ArticleNo: "45", Text: "A private limited company may be formed", DocumentTitle: "Commercial Code", ChunkID: legal.ChunkID("c3")},
	}
}

func TestLLMRerankerReordersByScore(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"scores":[0.2,0.1,0.95]}`}}
	reranker := NewLLMReranker(llm)

	got, err := reranker.Rerank(context.Background(), "how do I form a company?", llmRerankResults(), 2)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].ArticleNo != "45" {
		t.Errorf("top result = article %s, want 45", got[0].ArticleNo)
	}
	if got[0].Score != 0.95 {
		t.Errorf("score = %v, want 0.95", got[0].Score)
	}
}

func TestLLMRerankerSendsAStructuredRequest(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"scores":[0.5,0.5,0.5]}`}}
	reranker := NewLLMReranker(llm)

	if _, err := reranker.Rerank(context.Background(), "how do I form a company?", llmRerankResults(), 3); err != nil {
		t.Fatalf("Rerank() = %v", err)
	}

	if llm.got.JSONSchema == nil {
		t.Error("rerank call did not constrain the response shape")
	}
	if !strings.Contains(llm.got.Messages[0].Content, "how do I form a company?") {
		t.Error("rerank prompt did not carry the query")
	}
	// Same contextualised form Cohere gets, not a bare span — an LLM judging
	// relevance needs the document/article header exactly as much as a
	// cross-encoder does.
	if !strings.Contains(llm.got.Messages[0].Content, "Commercial Code") ||
		!strings.Contains(llm.got.Messages[0].Content, "Article 45") {
		t.Errorf("prompt sent without provenance: %q", llm.got.Messages[0].Content)
	}
}

func TestLLMRerankerHandlesEmptyCandidates(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"scores":[]}`}}
	reranker := NewLLMReranker(llm)

	got, err := reranker.Rerank(context.Background(), "q", nil, 5)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for no candidates", got)
	}
}

// A wrong count is exactly as dangerous here as Cohere's out-of-range index
// check exists to prevent: a score silently attached to the wrong provision.
// Rejecting outright, rather than guessing an alignment, is the safe failure
// — and internal/rag/sources.go's rerankTop already degrades any reranker
// error to fusion order, so this is not a fatal path for the answer.
func TestLLMRerankerRejectsAWrongScoreCount(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"scores":[0.5,0.5]}`}} // 2 scores, 3 candidates
	reranker := NewLLMReranker(llm)

	if _, err := reranker.Rerank(context.Background(), "q", llmRerankResults(), 3); err == nil {
		t.Error("Rerank() accepted a mismatched score count")
	}
}

func TestLLMRerankerRejectsMalformedJSON(t *testing.T) {
	llm := &stubLLM{chunks: []string{"not json"}}
	reranker := NewLLMReranker(llm)

	if _, err := reranker.Rerank(context.Background(), "q", llmRerankResults(), 3); err == nil {
		t.Error("Rerank() accepted a malformed response")
	}
}

func TestLLMRerankerSurfacesProviderErrors(t *testing.T) {
	llm := &stubLLM{err: context.DeadlineExceeded}
	reranker := NewLLMReranker(llm)

	if _, err := reranker.Rerank(context.Background(), "q", llmRerankResults(), 3); err == nil {
		t.Error("Rerank() ignored a provider error")
	}
}

func TestLLMRerankerCapsCandidatesJudged(t *testing.T) {
	many := make([]search.Result, llmRerankMaxCandidates+10)
	scores := make([]string, len(many))
	for i := range many {
		many[i] = search.Result{ArticleNo: "x", DocumentTitle: "Code", ChunkID: legal.ChunkID("c")}
		scores[i] = "0.1"
	}
	// Only the first llmRerankMaxCandidates scores are needed; the stub
	// returns exactly that many so a request for more than the cap fails
	// clearly rather than silently, proving the cap was actually applied.
	llm := &stubLLM{chunks: []string{"{\"scores\":[" + strings.Join(scores[:llmRerankMaxCandidates], ",") + "]}"}}
	reranker := NewLLMReranker(llm)

	got, err := reranker.Rerank(context.Background(), "q", many, 0)
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	if len(got) != llmRerankMaxCandidates {
		t.Errorf("judged/returned %d candidates, want the cap of %d", len(got), llmRerankMaxCandidates)
	}
}

func TestLLMRerankerName(t *testing.T) {
	llm := &stubLLM{}
	if got := NewLLMReranker(llm).Name(); !strings.HasPrefix(got, "llm:") {
		t.Errorf("Name() = %q, want an \"llm:\" prefix", got)
	}
}
