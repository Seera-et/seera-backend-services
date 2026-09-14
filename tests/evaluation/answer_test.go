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
	// isRelevant requires a title match before it will call anything relevant,
	// so a stale expected title does not merely skew these metrics — it drives
	// every one of them to zero while looking like a retrieval failure.
	assertExpectedTitlesExist(t, db, dataset)

	// Overall plus one bucket per language. The plan's exit gate sets different
	// targets for each (Precision@5 >= 0.80 overall, >= 0.65 Amharic), because
	// the risk it guards against is precisely that English carries the average
	// while Amharic retrieval quietly fails — which a single blended number
	// cannot show. Amharic retrieval is the documented reason the embedder-swap
	// path in §2.1 exists at all.
	overall := &retrievalTally{}
	byLanguage := map[string]*retrievalTally{}

	// Split by how specific the ground truth is, which matters more than it
	// sounds. isRelevant can only check a document title when a case carries no
	// expected article numbers, and the live corpus is 824 Commercial Code
	// chunks out of 879 (measured 2026-09-03) — 94% one document. So a
	// document-level case scores near-perfect Precision@5 whatever retrieval
	// does, and a blended P@5 reports corpus concentration rather than
	// retrieval quality. Only the article-level bucket is evidence.
	byGranularity := map[string]*retrievalTally{}

	var cases []retrievalCase

	for _, tc := range dataset.Cases {
		if tc.ShouldAbstain {
			continue // nothing should be relevant; retrieval quality is not the question here.
		}

		language := tc.Language
		if language == "" {
			language = "unspecified"
		}
		if byLanguage[language] == nil {
			byLanguage[language] = &retrievalTally{}
		}
		perLanguage := byLanguage[language]

		granularity := "document-level"
		if len(tc.ExpectedArticleNos) > 0 {
			granularity = "article-level"
		}
		if byGranularity[granularity] == nil {
			byGranularity[granularity] = &retrievalTally{}
		}
		perGranularity := byGranularity[granularity]

		overall.total++
		perLanguage.total++
		perGranularity.total++

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		fused, err := searchSvc.Search(ctx, search.Query{Text: tc.Question, Limit: 50})
		cancel()
		if err != nil {
			t.Errorf("case %q: search: %v", tc.ID, err)
			continue
		}

		result := retrievalCase{ID: tc.ID, Question: tc.Question, Language: language}

		result.Granularity = granularity

		if anyRelevant(fused, tc) {
			overall.fusedRecallHits++
			perLanguage.fusedRecallHits++
			perGranularity.fusedRecallHits++
			result.FoundInFused = true
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
			overall.rerankRecallHits++
			perLanguage.rerankRecallHits++
			perGranularity.rerankRecallHits++
			result.FoundInTop10 = true
		}

		if rank := firstRelevantRank(reranked, tc); rank > 0 {
			overall.reciprocalRankSum += 1.0 / float64(rank)
			perLanguage.reciprocalRankSum += 1.0 / float64(rank)
			perGranularity.reciprocalRankSum += 1.0 / float64(rank)
			result.FirstRelevantRank = rank
			if rank == 1 {
				overall.hit1++
				perLanguage.hit1++
				perGranularity.hit1++
			}
			if rank <= 3 {
				overall.hit3++
				perLanguage.hit3++
				perGranularity.hit3++
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
			p := float64(relevant) / float64(len(five))
			overall.precisionSum += p
			perLanguage.precisionSum += p
			perGranularity.precisionSum += p
			result.PrecisionAt5 = p
		}

		cases = append(cases, result)
	}

	if overall.total == 0 {
		t.Fatal("no non-abstain cases in the dataset — nothing to measure")
	}

	// The printed table docs/QA_FEATURE_PLAN.md §8 asks for, so a run is
	// readable without opening the JSON.
	languages := make([]string, 0, len(byLanguage))
	for language := range byLanguage {
		languages = append(languages, language)
	}
	sort.Strings(languages)

	t.Logf("%-12s %5s %10s %10s %10s %6s %6s %6s", "bucket", "n", "R@50", "R@10", "P@5", "MRR", "H@1", "H@3")
	t.Logf("%s", strings.Repeat("-", 72))
	logTally(t, "overall", overall)
	for _, language := range languages {
		logTally(t, language, byLanguage[language])
	}
	for _, granularity := range []string{"article-level", "document-level"} {
		if bucket := byGranularity[granularity]; bucket != nil {
			logTally(t, granularity, bucket)
		}
	}

	report := retrievalReport{
		Overall: overall.metrics("overall"),
		Cases:   cases,
	}
	for _, language := range languages {
		report.ByLanguage = append(report.ByLanguage, byLanguage[language].metrics(language))
	}
	for _, granularity := range []string{"article-level", "document-level"} {
		if bucket := byGranularity[granularity]; bucket != nil {
			report.ByGranularity = append(report.ByGranularity, bucket.metrics(granularity))
		}
	}
	writeReport(t, "retrieval", report)

	// The headline number is not trustworthy while most cases are
	// document-level, and this says so rather than letting a passing P@5 be
	// read as evidence retrieval works. On a corpus that is 94% one document,
	// a document-level case scores near 1.0 no matter what retrieval returns.
	articleLevel := byGranularity["article-level"]
	documentLevel := byGranularity["document-level"]
	if documentLevel != nil && documentLevel.total > 0 {
		share := float64(documentLevel.total) / float64(overall.total) * 100
		t.Logf("WARNING: %.0f%% of cases (%d/%d) carry document-level ground truth only. "+
			"Their Precision@5 measures how concentrated the corpus is, not how good retrieval is — "+
			"any chunk from the right document counts as a hit. Read the article-level bucket instead, "+
			"and fill in expected_article_nos as answers are verified against a real run.",
			share, documentLevel.total, overall.total)
	}
	if articleLevel == nil || articleLevel.total < 10 {
		n := 0
		if articleLevel != nil {
			n = articleLevel.total
		}
		t.Logf("WARNING: only %d case(s) have article-level ground truth, so the plan's "+
			"Precision@5 >= 0.80 gate is not yet meaningfully measurable.", n)
	}

	// A wide margin, deliberately: this dataset is small and its ground truth
	// is mostly document-level (see dataset.go), so this is a regression
	// alarm — "retrieval broke" — not a precision target to chase. The plan's
	// real gate (P@5 >= 0.80 overall, >= 0.65 Amharic) is reported above and
	// watched against the committed baseline rather than enforced here, for
	// the same reason TestAnswerQuality gates only on abstention: a strict
	// threshold on 24 mostly document-level cases would be measuring the
	// dataset's completeness, not retrieval's quality.
	if overall.recallAt50() < 0.5 {
		t.Errorf("Recall@50 %.0f%%, want at least 50%% — retrieval is not finding the right document at all",
			overall.recallAt50()*100)
	}

	// Amharic is the documented risk (§2.1, §10): the corpus is half Amharic by
	// intent, and a blended average hides an Amharic collapse behind English
	// results. Reported as its own line so the gate is visible even while the
	// bucket is too small to enforce.
	if am := byLanguage["am"]; am == nil || am.total == 0 {
		t.Logf("WARNING: no Amharic cases ran, so the Amharic P@5 >= 0.65 gate is unmeasured. " +
			"The corpus currently holds no Amharic chunks; see docs/CORPUS.md.")
	} else if am.total < 5 {
		t.Logf("WARNING: only %d Amharic case(s); P@5=%.0f%% is not a meaningful measurement against the 65%% gate",
			am.total, am.precisionAt5()*100)
	}
}

// retrievalTally accumulates one bucket's counters — overall, or one language.
type retrievalTally struct {
	total                             int
	fusedRecallHits, rerankRecallHits int
	precisionSum                      float64
	reciprocalRankSum                 float64
	hit1, hit3                        int
}

func (r *retrievalTally) recallAt50() float64 {
	return ratio(r.fusedRecallHits, r.total)
}
func (r *retrievalTally) recallAt10() float64 {
	return ratio(r.rerankRecallHits, r.total)
}
func (r *retrievalTally) precisionAt5() float64 { return mean(r.precisionSum, r.total) }
func (r *retrievalTally) mrr() float64          { return mean(r.reciprocalRankSum, r.total) }
func (r *retrievalTally) hitAt1() float64       { return ratio(r.hit1, r.total) }
func (r *retrievalTally) hitAt3() float64       { return ratio(r.hit3, r.total) }

func ratio(hits, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

func mean(sum float64, total int) float64 {
	if total == 0 {
		return 0
	}
	return sum / float64(total)
}

func (r *retrievalTally) metrics(bucket string) retrievalMetrics {
	return retrievalMetrics{
		Bucket:       bucket,
		Cases:        r.total,
		RecallAt50:   r.recallAt50(),
		RecallAt10:   r.recallAt10(),
		PrecisionAt5: r.precisionAt5(),
		MRR:          r.mrr(),
		HitAt1:       r.hitAt1(),
		HitAt3:       r.hitAt3(),
	}
}

func logTally(t *testing.T, bucket string, r *retrievalTally) {
	t.Helper()
	t.Logf("%-12s %5d %9.0f%% %9.0f%% %9.0f%% %6.2f %5.0f%% %5.0f%%",
		bucket, r.total,
		r.recallAt50()*100, r.recallAt10()*100, r.precisionAt5()*100,
		r.mrr(), r.hitAt1()*100, r.hitAt3()*100)
}

// retrievalMetrics is one bucket's scores, as written to the run report.
type retrievalMetrics struct {
	Bucket       string  `json:"bucket"`
	Cases        int     `json:"cases"`
	RecallAt50   float64 `json:"recall_at_50"`
	RecallAt10   float64 `json:"recall_at_10"`
	PrecisionAt5 float64 `json:"precision_at_5"`
	MRR          float64 `json:"mrr"`
	HitAt1       float64 `json:"hit_at_1"`
	HitAt3       float64 `json:"hit_at_3"`
}

// retrievalCase is one question's outcome, kept per-case so a regression can be
// traced to the questions that moved rather than only to an average that fell.
type retrievalCase struct {
	ID                string  `json:"id"`
	Question          string  `json:"question"`
	Language          string  `json:"language"`
	Granularity       string  `json:"granularity"`
	FoundInFused      bool    `json:"found_in_fused"`
	FoundInTop10      bool    `json:"found_in_top10"`
	FirstRelevantRank int     `json:"first_relevant_rank"`
	PrecisionAt5      float64 `json:"precision_at_5"`
}

// retrievalReport is what lands in results/retrieval_latest.json. Committed, so
// a change to chunking, fusion or reranking is measured against a baseline
// rather than argued about — docs/QA_FEATURE_PLAN.md §8.
type retrievalReport struct {
	Overall    retrievalMetrics   `json:"overall"`
	ByLanguage []retrievalMetrics `json:"by_language"`
	// ByGranularity separates cases with article-level ground truth from those
	// checked only by document title. On a corpus dominated by one document the
	// latter cannot measure precision, so the two must never be blended into a
	// single headline number.
	ByGranularity []retrievalMetrics `json:"by_granularity"`
	Cases         []retrievalCase    `json:"cases"`
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
	Language        string `json:"language,omitempty"`
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
	assertExpectedTitlesExist(t, db, dataset)

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
			Language:      tc.Language,
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

// assertExpectedTitlesExist fails when a case expects a document title that no
// document in the corpus has.
//
// This exists because the failure it catches is silent. TitleMatched is scored
// by comparing a citation's DocumentTitle against the dataset's expected title;
// a title that matches nothing simply never sets the flag, and the metric reads
// as "retrieval found the wrong document" rather than "this annotation is
// stale". Checked 2026-09-03: all 24 annotated cases expected "Commercial Code
// of Ethiopia" and "Constitution of Ethiopia (1931)" while the corpus held
// "Commercial Code of Ethiopia (Proclamation No. 1243/2021)" and "The 1931
// Constitution of Ethiopia", so title matching had been 0/28 since the dataset
// was written and nothing said so. qa_v1.yaml's own header had warned that the
// titles were transcribed from a plan document rather than a live row.
//
// Deliberately exact, matching the comparison it guards, and deliberately
// fatal. Ground truth that quietly measures nothing is worse than a failing
// test: it produces a number that looks like a result.
func assertExpectedTitlesExist(t *testing.T, db *database.Pool, dataset answerDataset) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.Query(ctx, `SELECT title FROM legal_documents`)
	if err != nil {
		t.Fatalf("read document titles: %v", err)
	}
	defer rows.Close()

	corpus := map[string]bool{}
	var known []string
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			t.Fatalf("scan document title: %v", err)
		}
		corpus[strings.ToLower(title)] = true
		known = append(known, title)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read document titles: %v", err)
	}
	if len(known) == 0 {
		t.Fatal("the corpus has no documents; ingest before evaluating")
	}

	stale := map[string][]string{}
	for _, tc := range dataset.Cases {
		for _, want := range tc.ExpectedDocumentTitles {
			if !corpus[strings.ToLower(want)] {
				stale[want] = append(stale[want], tc.ID)
			}
		}
	}
	if len(stale) == 0 {
		return
	}

	sort.Strings(known)
	for want, ids := range stale {
		t.Errorf("no document titled %q is in the corpus, so %d case(s) can never match it: %s",
			want, len(ids), strings.Join(ids, ", "))
	}
	t.Fatalf("stale ground truth: correct qa_v1.yaml against the titles actually present:\n  %s",
		strings.Join(known, "\n  "))
}
