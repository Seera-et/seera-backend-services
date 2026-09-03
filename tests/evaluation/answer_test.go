//go:build eval

// Package evaluation measures the answer path against a fixed dataset.
//
// This is the answer-quality harness docs/QA_FEATURE_PLAN.md §8 specified and
// that was never built — only the routing eval existed before it. Split into
// two functions so a bad result is never ambiguously retrieval's fault or
// generation's: TestRetrievalQuality never generates, TestAnswerQuality never
// bypasses the reranker production actually uses.
//
//	set -a && source .env && set +a
//	go test -tags eval ./tests/evaluation/... -run TestRetrievalQuality -v
//	go test -tags eval ./tests/evaluation/... -run TestAnswerQuality -v
package evaluation

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/citations"
	"github.com/Seera-et/seera-backend-services/internal/database"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/rag"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// evalLogger discards everything: these tests report through t.Log and the
// written report, not through the application's structured logging.
func evalLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildEmbedder needs only GEMINI_API_KEY. Gemini, not OpenAI, because it is
// what the corpus was actually embedded with in every documented run
// (docs/QA_FEATURE_PLAN.md §2.1) — comparing against a different embedding
// space would not measure this corpus's retrieval quality at all.
func buildEmbedder(t *testing.T) ai.Embedder {
	t.Helper()
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY is not set; retrieval evaluation needs real embeddings")
	}
	embedder, err := ai.NewGeminiEmbedder(key)
	if err != nil {
		t.Fatalf("build embedder: %v", err)
	}
	return embedder
}

// connectDB needs DATABASE_URL pointing at an already-ingested corpus. This
// cannot embed a corpus from scratch on every run — re-embedding a code's
// worth of articles would blow the free-tier daily quota documented in
// docs/QA_FEATURE_PLAN.md §7 ("First real run") — so it depends on a fixture
// the operator has already ingested, the same way routing_test.go depends on a
// live model rather than a mock.
func connectDB(t *testing.T) *database.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; answer-quality evaluation needs an ingested corpus")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// buildEvalLLM mirrors cmd/api/main.go's buildLLM: Gemini is the model this
// project's evaluation baseline is measured against, so it stays primary here
// too — this only adds resilience against exactly what blocked the last
// edgecases run (the free-tier daily quota), not a different model to
// evaluate against. If OPENROUTER_API_KEY is unset, behavior is unchanged
// from a bare *ai.GeminiLLM — including still failing the test outright when
// Gemini is exhausted, so a run without OpenRouter still tells the truth
// about Gemini's daily quota rather than silently degrading to something else.
func buildEvalLLM(t *testing.T, gemini *ai.GeminiLLM) ai.LLM {
	t.Helper()
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return gemini
	}
	opts := []ai.OpenRouterOption{}
	if model := os.Getenv("OPENROUTER_MODEL"); model != "" {
		// Not reading this was a real gap, not a style choice: production
		// (cmd/api/main.go's buildLLM) has read OPENROUTER_MODEL since the
		// fallback was added, so a run without this silently evaluated a
		// different, less reliable model than the one actually deployed —
		// found 2026-08-28 when a run still hit the free-tier auto-router
		// after OPENROUTER_MODEL was set to a paid model in .env.
		opts = append(opts, ai.WithOpenRouterModel(model))
	}
	fallback, err := ai.NewOpenRouterLLM(key, opts...)
	if err != nil {
		t.Logf("OpenRouter fallback unavailable (%v), evaluation runs on Gemini alone", err)
		return gemini
	}
	t.Logf("OpenRouter fallback enabled for this run (model: %s) — a result produced via it "+
		"is evidence about the fallback path, not about Gemini's prompt compliance", fallback.Model())
	return ai.NewFallbackLLM(gemini, fallback, evalLogger())
}

// buildRerankerForEval mirrors cmd/api/main.go's buildReranker, duplicated
// here for the same reason routing_test.go duplicates router.New's wiring
// rather than importing cmd/api: buildReranker is unexported, package main.
// The threshold is only meaningful paired with a calibrated scorer.
//
// llm is required, not optional: without Cohere, production reranks with
// rag.LLMReranker (added 2026-08-28) rather than PassthroughReranker, and an
// eval that still measured against the passthrough would be evaluating a
// pipeline nobody runs. TestRetrievalQuality building an LLM now, when it
// previously needed only embeddings, is the honest consequence of that
// change — retrieval quality now depends on the same reranker generation does.
func buildRerankerForEval(t *testing.T, llm ai.LLM) (rag.Reranker, float64) {
	t.Helper()
	key := os.Getenv("COHERE_API_KEY")
	if key == "" {
		return rag.NewLLMReranker(llm), rag.LLMRerankMinScore
	}
	reranker, err := rag.NewCohereReranker(key)
	if err != nil {
		t.Logf("Cohere reranker unavailable (%v), using the LLM reranker instead", err)
		return rag.NewLLMReranker(llm), rag.LLMRerankMinScore
	}
	return reranker, 0.15
}

// retrievalCase is one dataset case reduced to what retrieval-only evaluation
// needs: whether anything in the fused/reranked list actually matches.
func isRelevant(r search.Result, tc answerCase) bool {
	titleMatch := len(tc.ExpectedDocumentTitles) == 0
	for _, title := range tc.ExpectedDocumentTitles {
		if strings.EqualFold(r.DocumentTitle, title) {
			titleMatch = true
			break
		}
	}
	if !titleMatch {
		return false
	}
	if len(tc.ExpectedArticleNos) == 0 {
		// Document-only ground truth (see dataset.go's comment on answerCase):
		// a title match is all this case can honestly check.
		return true
	}
	for _, no := range tc.ExpectedArticleNos {
		if r.ArticleNo == no {
			return true
		}
	}
	return false
}

func TestRetrievalQuality(t *testing.T) {
	embedder := buildEmbedder(t)
	db := connectDB(t)

	gemini, err := ai.NewGeminiLLM(os.Getenv("GEMINI_API_KEY"))
	if err != nil {
		t.Fatalf("build LLM: %v", err)
	}
	llm := buildEvalLLM(t, gemini)

	searchSvc, err := search.NewService(db.Pool, embedder, search.WithTranslation(llm, evalLogger()))
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	reranker, _ := buildRerankerForEval(t, llm)

	dataset, err := LoadAnswerDataset()
	if err != nil {
		t.Fatalf("dataset: %v", err)
	}

	var (
		total                             int
		fusedRecallHits, rerankRecallHits int
		precisionSum                      float64
		reciprocalRankSum                 float64
		hit1, hit3                        int
	)

	for _, tc := range dataset.Cases {
		if tc.ShouldAbstain {
			continue // nothing should be relevant; retrieval quality is not the question here.
		}
		total++

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		fused, err := searchSvc.Search(ctx, search.Query{Text: tc.Question, Limit: 50})
		cancel()
		if err != nil {
			t.Errorf("case %q: search: %v", tc.ID, err)
			continue
		}

		if anyRelevant(fused, tc) {
			fusedRecallHits++
		}

		rerankCtx, rerankCancel := context.WithTimeout(context.Background(), 15*time.Second)
		reranked, err := reranker.Rerank(rerankCtx, tc.Question, fused, 10)
		rerankCancel()
		if err != nil {
			t.Logf("case %q: reranker failed (%v), using fusion order", tc.ID, err)
			reranked = fused
			if len(reranked) > 10 {
				reranked = reranked[:10]
			}
		}
		if anyRelevant(reranked, tc) {
			rerankRecallHits++
		}

		if rank := firstRelevantRank(reranked, tc); rank > 0 {
			reciprocalRankSum += 1.0 / float64(rank)
			if rank == 1 {
				hit1++
			}
			if rank <= 3 {
				hit3++
			}
		}

		five := reranked
		if len(five) > 5 {
			five = five[:5]
		}
		relevant := 0
		for _, r := range five {
			if isRelevant(r, tc) {
				relevant++
			}
		}
		if len(five) > 0 {
			precisionSum += float64(relevant) / float64(len(five))
		}
	}

	if total == 0 {
		t.Fatal("no non-abstain cases in the dataset — nothing to measure")
	}

	recallAt50 := float64(fusedRecallHits) / float64(total)
	recallAt10 := float64(rerankRecallHits) / float64(total)
	precisionAt5 := precisionSum / float64(total)
	mrr := reciprocalRankSum / float64(total)
	hitAt1 := float64(hit1) / float64(total)
	hitAt3 := float64(hit3) / float64(total)

	t.Logf("retrieval quality over %d cases: Recall@50(fused)=%.0f%%, Recall@10(reranked)=%.0f%%, Precision@5=%.0f%%, MRR=%.2f, Hit@1=%.0f%%, Hit@3=%.0f%%",
		total, recallAt50*100, recallAt10*100, precisionAt5*100, mrr, hitAt1*100, hitAt3*100)

	// A wide margin, deliberately: this dataset is small and its ground truth
	// is mostly document-level (see dataset.go), so this is a regression
	// alarm — "retrieval broke" — not a precision target to chase.
	if recallAt50 < 0.5 {
		t.Errorf("Recall@50 %.0f%%, want at least 50%% — retrieval is not finding the right document at all",
			recallAt50*100)
	}
}

func anyRelevant(results []search.Result, tc answerCase) bool {
	for _, r := range results {
		if isRelevant(r, tc) {
			return true
		}
	}
	return false
}

// firstRelevantRank returns the 1-based rank of the first relevant result in
// results, or 0 if none is relevant. The basis for MRR and Hit@k: unlike
// Recall/Precision at a fixed cutoff, this captures *how high* a relevant
// result ranked, not just whether one appeared somewhere in a fixed-size
// window.
func firstRelevantRank(results []search.Result, tc answerCase) int {
	for i, r := range results {
		if isRelevant(r, tc) {
			return i + 1
		}
	}
	return 0
}

func TestFirstRelevantRank(t *testing.T) {
	tc := answerCase{ExpectedDocumentTitles: []string{"Commercial Code"}, ExpectedArticleNos: []string{"499"}}
	other := search.Result{DocumentTitle: "Commercial Code", ArticleNo: "1"}
	match := search.Result{DocumentTitle: "Commercial Code", ArticleNo: "499"}

	if got := firstRelevantRank(nil, tc); got != 0 {
		t.Errorf("firstRelevantRank(nil) = %d, want 0", got)
	}
	if got := firstRelevantRank([]search.Result{other, other, other}, tc); got != 0 {
		t.Errorf("firstRelevantRank(no match) = %d, want 0", got)
	}
	if got := firstRelevantRank([]search.Result{match, other, other}, tc); got != 1 {
		t.Errorf("firstRelevantRank(match at 0) = %d, want 1", got)
	}
	if got := firstRelevantRank([]search.Result{other, other, match}, tc); got != 3 {
		t.Errorf("firstRelevantRank(match at 2) = %d, want 3", got)
	}
}

// answerResult is one case's outcome against a real, generating answer path.
type answerResult struct {
	ID              string `json:"id"`
	Question        string `json:"question"`
	ShouldAbstain   bool   `json:"should_abstain"`
	Kind            string `json:"kind"`
	Grounded        bool   `json:"grounded"`
	CitationOK      bool   `json:"citation_ok,omitempty"`
	TitleMatched    bool   `json:"title_matched"`
	AbstentionOK    bool   `json:"abstention_ok"`
	FirstTokenMS    int64  `json:"first_token_ms"`
	ConfidenceLevel string `json:"confidence_level,omitempty"`
	Problem         string `json:"problem,omitempty"`
}

func TestAnswerQuality(t *testing.T) {
	embedder := buildEmbedder(t)
	db := connectDB(t)

	key := os.Getenv("GEMINI_API_KEY")
	gemini, err := ai.NewGeminiLLM(key)
	if err != nil {
		t.Fatalf("build LLM: %v", err)
	}
	llm := buildEvalLLM(t, gemini)

	searchSvc, err := search.NewService(db.Pool, embedder, search.WithTranslation(llm, evalLogger()))
	if err != nil {
		t.Fatalf("build search service: %v", err)
	}
	reranker, minRerankScore := buildRerankerForEval(t, llm)

	citationSvc := citations.NewService(citations.NewRepository(db.Pool), evalLogger())

	ragCfg := rag.DefaultConfig()
	ragCfg.MinRerankScore = minRerankScore
	// Mirrors cmd/api/main.go's ragCfg.MultiHop = cfg.MultiHop. Not read
	// before 2026-08-28: this test built its rag.Service with MultiHop always
	// at its zero-value false, so ANSWER_MULTI_HOP=true had no effect on it —
	// the very comparison Gap 3 exists to run.
	ragCfg.MultiHop = os.Getenv("ANSWER_MULTI_HOP") == "true"
	svc := rag.NewService(searchSvc, reranker, llm, ragCfg, evalLogger(),
		rag.WithSources(citationSvc),
		rag.WithArticles(searchSvc))
	t.Logf("multi_hop=%v", ragCfg.MultiHop)

	dataset, err := LoadAnswerDataset()
	if err != nil {
		t.Fatalf("dataset: %v", err)
	}

	var (
		results                            []answerResult
		grounded, groundable               int
		citationHits, citationTotal        int
		abstentionCorrect, abstentionTotal int
		firstTokens                        []int64
	)

	for _, tc := range dataset.Cases {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()

		var final *rag.Result
		var citationsGot []rag.Citation
		err := svc.Answer(ctx, rag.Question{
			Text:     tc.Question,
			Language: legal.Language(tc.Language),
		}, func(e rag.Event) error {
			if e.Type == rag.EventDone {
				final = e.Result
				citationsGot = e.Citations
			}
			return nil
		})
		cancel()
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("case %q: Answer: %v", tc.ID, err)
			continue
		}
		if final == nil {
			t.Errorf("case %q: no done event", tc.ID)
			continue
		}

		res := answerResult{
			ID:            tc.ID,
			Question:      tc.Question,
			ShouldAbstain: tc.ShouldAbstain,
			Kind:          string(final.Kind),
			Grounded:      final.Grounded,
			FirstTokenMS:  final.Timings.FirstToken,
		}
		if final.Confidence != nil {
			res.ConfidenceLevel = string(final.Confidence.Level)
		}

		if tc.ShouldAbstain {
			abstentionTotal++
			// Either outcome means "did not fabricate" — the dataset predates
			// KindInsufficient, and a literal should_abstain check would wrongly
			// punish the more honest of two safe outcomes.
			res.AbstentionOK = final.Kind == rag.KindAbstention || final.Kind == rag.KindInsufficient
			if res.AbstentionOK {
				abstentionCorrect++
			} else {
				res.Problem = "should have abstained or reported insufficient sources"
			}
			results = append(results, res)
			continue
		}

		firstTokens = append(firstTokens, elapsed.Milliseconds())

		for _, title := range tc.ExpectedDocumentTitles {
			for _, c := range citationsGot {
				if strings.EqualFold(c.DocumentTitle, title) {
					res.TitleMatched = true
				}
			}
		}

		if len(tc.ExpectedArticleNos) > 0 {
			citationTotal++
			for _, want := range tc.ExpectedArticleNos {
				for _, c := range citationsGot {
					if c.ArticleNo == want {
						res.CitationOK = true
					}
				}
			}
			if res.CitationOK {
				citationHits++
			} else {
				res.Problem = "did not cite an expected article number"
			}
		}

		if final.Kind == rag.KindLegal || final.Kind == rag.KindInsufficient {
			groundable++
			if final.Grounded {
				grounded++
			}
		}

		results = append(results, res)
	}

	t.Logf("answer quality over %d cases:", len(results))
	if abstentionTotal > 0 {
		t.Logf("  abstention correctness: %d/%d", abstentionCorrect, abstentionTotal)
	}
	if citationTotal > 0 {
		t.Logf("  citation accuracy (verified article numbers only): %d/%d", citationHits, citationTotal)
	}
	if groundable > 0 {
		t.Logf("  groundedness: %d/%d", grounded, groundable)
	}
	if len(firstTokens) > 0 {
		sort.Slice(firstTokens, func(i, j int) bool { return firstTokens[i] < firstTokens[j] })
		t.Logf("  TTFT p50=%dms p95=%dms (non-abstained cases only)",
			percentile(firstTokens, 0.5), percentile(firstTokens, 0.95))
	}

	for _, res := range results {
		if res.Problem != "" {
			t.Logf("  %s: %s", res.ID, res.Problem)
		}
	}

	writeReport(t, "answer", results)

	// The one hard gate: abstention correctness is what stands between this
	// product and a fabricated legal claim. Everything else is reported and
	// watched, not failed on — the dataset's own ground truth is partly
	// unverified (see dataset.go), so a strict gate on it would be measuring
	// this dataset's completeness, not the answer path's quality.
	if abstentionTotal > 0 && abstentionCorrect < abstentionTotal {
		t.Errorf("%d/%d should-abstain case(s) got an answer instead of abstaining or reporting insufficient sources",
			abstentionTotal-abstentionCorrect, abstentionTotal)
	}
}

// percentile expects a sorted slice. Nearest-rank, not interpolated — good
// enough for a handful of cases and simpler to reason about than a fit.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}
