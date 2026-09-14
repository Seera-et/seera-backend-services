package rag

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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

// The reranker's prompt is the only part of the time-to-first-token budget
// whose size depends on which articles retrieval happened to return. Bounding
// each candidate is what makes that budget predictable — measured against the
// live corpus, chunks reach 1,490 characters at p95 and 3,741 at the maximum.
func TestRerankExcerptBoundsLongProvisions(t *testing.T) {
	long := strings.Repeat("a provision about company formation ", 200) // ~7200 chars

	got := rerankExcerpt(search.Result{
		DocumentTitle: "Commercial Code",
		ArticleNo:     "265",
		ArticleTitle:  "Registration",
		Text:          long,
	})

	if len([]rune(got)) > maxCandidateRunes+200 {
		t.Errorf("excerpt is %d runes, want the text bounded near %d",
			len([]rune(got)), maxCandidateRunes)
	}
	// The header is what makes a bare span judgeable, so it must survive.
	if !strings.Contains(got, "Commercial Code") || !strings.Contains(got, "265") {
		t.Errorf("excerpt dropped the article header:\n%s", got)
	}
	if !strings.Contains(got, "…") {
		t.Error("a truncated excerpt should show that it was cut")
	}
}

func TestRerankExcerptLeavesShortProvisionsWhole(t *testing.T) {
	text := "The capital of a private limited company may not be less than fifteen thousand Birr."

	got := rerankExcerpt(search.Result{DocumentTitle: "Commercial Code", ArticleNo: "496", Text: text})

	if !strings.Contains(got, text) {
		t.Errorf("a short provision was altered:\n%s", got)
	}
	if strings.Contains(got, "…") {
		t.Error("a short provision should not be marked as truncated")
	}
}

// Amharic is three bytes per character. A byte-wise bound would cut Ethiopic
// text at a third of the intended length and could split a character.
func TestRerankExcerptCountsAmharicByCharacter(t *testing.T) {
	amharic := strings.Repeat("የንግድ ማህበር ምዝገባ ", 200)

	got := rerankExcerpt(search.Result{DocumentTitle: "የንግድ ሕግ", ArticleNo: "265", Text: amharic})

	runes := len([]rune(got))
	if runes < maxCandidateRunes/2 {
		t.Errorf("Amharic excerpt is only %d runes — bounded by bytes, not characters", runes)
	}
	if !utf8.ValidString(got) {
		t.Error("excerpt split a multi-byte character")
	}
}

// The generator and the citations must still see the whole provision: this
// bounds what the reranker reads, never what the answer is built from.
func TestRerankExcerptDoesNotMutateTheResult(t *testing.T) {
	full := strings.Repeat("x", 3000)
	result := search.Result{DocumentTitle: "Commercial Code", ArticleNo: "265", Text: full}

	_ = rerankExcerpt(result)

	if result.Text != full {
		t.Error("rerankExcerpt shortened the caller's result; the generator would lose evidence")
	}
}

// Quantifies what the bound buys, using the live corpus's measured shape
// (2026-09-03: 879 chunks, mean 607 characters, p95 1,490, max 3,741).
//
// The point is not the exact ratio but that the prompt stops depending on
// which articles retrieval happened to return: before, a question that pulled
// twenty long provisions cost roughly twice one that pulled twenty average
// ones, and time-to-first-token moved with it.
func TestRerankPromptSizeIsBoundedRegardlessOfCorpus(t *testing.T) {
	build := func(chars int) int {
		candidates := make([]search.Result, llmRerankMaxCandidates)
		for i := range candidates {
			candidates[i] = search.Result{
				DocumentTitle: "Commercial Code of Ethiopia",
				ArticleNo:     "265",
				ArticleTitle:  "Registration of the Company",
				Text:          strings.Repeat("x", chars),
			}
		}
		return len([]rune(buildRerankPrompt("How do I register a company?", candidates)))
	}

	average := build(607)
	p95 := build(1490)
	worst := build(3741)

	t.Logf("rerank prompt runes — average chunk: %d, p95: %d, largest: %d", average, p95, worst)

	// Every bucket lands on the same bounded size, so the worst case is the
	// normal case rather than five times it.
	if worst != p95 || p95 != average {
		t.Errorf("prompt size still varies with chunk length: avg=%d p95=%d worst=%d",
			average, p95, worst)
	}

	// Sanity bound: twenty candidates at 600 runes plus headers and the
	// question. Unbounded, the largest case alone would have been ~75,000.
	if ceiling := llmRerankMaxCandidates * (maxCandidateRunes + 200); worst > ceiling {
		t.Errorf("bounded prompt is %d runes, want at most %d", worst, ceiling)
	}
}
