package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// llmRerankMaxCandidates bounds how many results one structured call judges.
// A listwise judgment in a single response degrades past this, and every
// candidate beyond it was already the weaker half of the fused order — RRF
// puts what multiple retrieval arms agree on first.
const llmRerankMaxCandidates = 20

// LLMRerankMinScore is the abstention threshold paired with this reranker, on
// the 0-1 relevance scale its prompt asks for. Unlike CohereRerankModel's
// 0.15 (a number this codebase can at least reason about, tuned against a
// specific calibrated scorer), this is a starting point, not a validated
// number: there is no eval baseline yet for an LLM-judged relevance scale.
// Revisit once tests/evaluation's answer-quality harness has run enough real
// questions through it to say whether 0.3 is too strict, too loose, or right.
const LLMRerankMinScore = 0.3

// LLMReranker scores candidates with one structured call to whatever LLM is
// already wired in.
//
// It exists to close a real gap: without Cohere configured, PassthroughReranker
// leaves MinRerankScore at zero, so shouldAbstain's only signal is "zero
// candidates" — a single, barely-relevant result always passes, because
// nothing calibrated ever told it otherwise. This reranker costs no new
// credential (the LLM already answering questions scores its own retrieval
// candidates) and gives shouldAbstain a real number to compare against.
//
// The trade is latency: one more network round trip inside the
// time-to-first-token budget, where a purpose-built cross-encoder would be
// faster and (likely) better-calibrated. That is exactly why cmd/api/main.go
// only reaches for this when Cohere is not configured, not unconditionally —
// same shape as ai.FallbackLLM preferring the primary provider.
type LLMReranker struct {
	llm ai.LLM
}

// NewLLMReranker builds the reranker around an already-configured LLM. There
// is no separate credential to fail on — if llm works, this works.
func NewLLMReranker(llm ai.LLM) *LLMReranker { return &LLMReranker{llm: llm} }

func (r *LLMReranker) Name() string { return "llm:" + r.llm.Model() }

const rerankSystemPrompt = `You score how well each numbered candidate provision answers a legal question.
You are not answering the question — you are judging retrieval, so that the
right provisions can be handed to the model that does.

Score each candidate from 0 to 1: 1 means it directly and specifically answers
the question; 0 means it is unrelated. A provision that touches the general
subject but does not answer this specific question belongs in the middle, not
at either end just because it is topically nearby — topical proximity is not
relevance.

Return exactly one score per candidate, in the order given. Say nothing else,
and never state what any provision means — that is not this call's job.`

var rerankResponseSchema = &ai.Schema{
	Type: "object",
	Properties: map[string]*ai.Schema{
		"scores": {
			Type:        "array",
			Description: "One relevance score per candidate, same order as listed, 0 (irrelevant) to 1 (directly answers the question).",
			Items:       &ai.Schema{Type: "number"},
		},
	},
	Required: []string{"scores"},
}

// Rerank scores each candidate's relevance to the query with one structured
// call and returns the topN, best first.
//
// Errors here are not special-cased for recovery — the same reason
// rag/sources.go's rerankTop already treats any reranker failure as
// "fall back to fusion order, not a wrong answer" applies to this
// implementation exactly as it does to Cohere's.
func (r *LLMReranker) Rerank(ctx context.Context, query string, results []search.Result, topN int) ([]search.Result, error) {
	if len(results) == 0 {
		return nil, nil
	}

	candidates := results
	if len(candidates) > llmRerankMaxCandidates {
		candidates = candidates[:llmRerankMaxCandidates]
	}

	raw, _, err := r.llm.Complete(ctx, ai.Request{
		System:     rerankSystemPrompt,
		Messages:   []ai.Message{{Role: ai.RoleUser, Content: buildRerankPrompt(query, candidates)}},
		MaxTokens:  64 + 12*len(candidates),
		JSONSchema: rerankResponseSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("llm rerank: %w", err)
	}

	scores, err := parseRerankScores(raw, len(candidates))
	if err != nil {
		return nil, fmt.Errorf("llm rerank: %w", err)
	}

	scored := make([]search.Result, len(candidates))
	copy(scored, candidates)
	for i := range scored {
		scored[i].Score = scores[i]
	}
	// Stable so that, among equal scores, the fused arms' own ranking (RRF
	// already rewards multi-arm agreement) still breaks the tie.
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })

	if topN > 0 && topN < len(scored) {
		scored = scored[:topN]
	}
	return scored, nil
}

// maxCandidateRunes bounds how much of each provision the reranker reads.
//
// This call sits inside the time-to-first-token budget, and its prompt is the
// only part of that budget whose size depends on which articles retrieval
// happened to return. Measured against the live corpus: chunks average 607
// characters but reach 1,490 at p95 and 3,741 at the maximum, so 20 candidates
// swing between roughly 3k and 7.5k tokens — and TTFT swung with it, p50 3.1s
// against a 3s target.
//
// 600 keeps the article header and the opening of the provision, which is
// where a legal chunk states what it governs. It is not a quality compromise
// invented here: a cross-encoder truncates long documents natively, so this
// makes the LLM reranker judge on roughly what Cohere's rerank-v3.5 would see
// rather than on strictly more.
//
// Only the reranker excerpts. The generator still receives every chunk in
// full — truncating evidence would be a different and much worse change.
const maxCandidateRunes = 600

// buildRerankPrompt reuses rerankDocument (rag/reranker.go) so the LLM judges
// the same contextualised text Cohere would — title and article header, not a
// bare span a cross-encoder or an LLM would find equally ambiguous.
func buildRerankPrompt(query string, candidates []search.Result) string {
	var b strings.Builder
	b.WriteString("Question: ")
	b.WriteString(query)
	b.WriteString("\n\nCandidates:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "\n%d.\n%s\n", i+1, rerankExcerpt(c))
	}
	return b.String()
}

// rerankExcerpt renders a candidate with its text bounded.
//
// Takes search.Result by value, so shortening Text here cannot reach the copy
// the generator and the citations are built from.
//
// Reuses history.go's truncateRunes, which counts runes rather than bytes —
// half the corpus is Amharic, where every character is three bytes, so a
// byte-wise cut would truncate Ethiopic text at a third of the intended length
// and could split a character in half.
func rerankExcerpt(r search.Result) string {
	r.Text = truncateRunes(r.Text, maxCandidateRunes)
	return rerankDocument(r)
}

func parseRerankScores(raw string, want int) ([]float64, error) {
	var payload struct {
		Scores []float64 `json:"scores"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return nil, fmt.Errorf("decode scores: %w", err)
	}
	if len(payload.Scores) != want {
		return nil, fmt.Errorf("got %d scores, want %d", len(payload.Scores), want)
	}
	return payload.Scores, nil
}
