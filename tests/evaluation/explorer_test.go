//go:build eval

// Explorer search quality — the gate for SRS AC-002.
//
// Separate from the answer evaluation on purpose. This never generates and
// never reranks: it measures whether the Explorer puts the right provision in
// front of a reader, which is a different question from whether an answer
// cites the right provision, and a blended number would hide either failure
// behind the other.
//
//	set -a && source .env && set +a
//	go test -tags eval ./tests/evaluation/... -run TestExplorerSearchQuality -v
//	go test -tags eval ./tests/evaluation/... -run TestExplorerModeSplit -v
package evaluation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/documents"
)

// AC-002's pass condition. Exact-search tests must succeed at least this often.
const explorerSuccessTarget = 0.95

// successAtK is how far down the list a hit still counts.
//
// Five, matching the Precision@5 the rest of the project is measured at and the
// number of results a reader actually looks at before re-phrasing.
const successAtK = 5

// Latency budget for corpus search, SRS §14.1 ("Hybrid search < 1 second").
const searchLatencyBudget = time.Second

func buildCatalogue(t *testing.T) *documents.Service {
	t.Helper()
	db := connectDB(t)
	repo, err := documents.NewRepository(db.Pool,
		documents.WithEmbedder(buildEmbedder(t)))
	if err != nil {
		t.Fatalf("build catalogue repository: %v", err)
	}
	return documents.NewService(repo)
}

type explorerTally struct {
	total int
	hits  int

	// rankPinned counts cases that asserted a rank and met it.
	rankCases  int
	rankPinned int

	// emptyCases are the out-of-corpus ones; emptyCorrect returned nothing.
	emptyCases   int
	emptyCorrect int

	slow      int
	worstTime time.Duration
}

func (e explorerTally) successRate() float64 {
	answerable := e.total - e.emptyCases
	if answerable == 0 {
		return 0
	}
	return float64(e.hits) / float64(answerable)
}

// TestExplorerSearchQuality runs every case in the default (hybrid) mode.
func TestExplorerSearchQuality(t *testing.T) {
	dataset, err := LoadExplorerDataset()
	if err != nil {
		t.Fatalf("explorer dataset: %v", err)
	}
	catalogue := buildCatalogue(t)

	var tally explorerTally
	for _, tc := range dataset.Cases {
		if tc.Adjacent {
			continue // measured by TestExplorerAdjacentTopics, not asserted here
		}
		tally.total++

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		result, err := catalogue.Search(ctx, documents.SearchFilter{
			Term:  tc.Query,
			Mode:  documents.SearchModeHybrid,
			Limit: successAtK,
		})
		elapsed := time.Since(start)
		cancel()

		if err != nil {
			t.Errorf("%s %q: search failed: %v", tc.ID, tc.Query, err)
			continue
		}

		if elapsed > searchLatencyBudget {
			tally.slow++
		}
		if elapsed > tally.worstTime {
			tally.worstTime = elapsed
		}

		if tc.ExpectEmpty {
			tally.emptyCases++
			if len(result.Hits) == 0 {
				tally.emptyCorrect++
				t.Logf("PASS %s  (nothing, correctly)  %v", tc.ID, elapsed.Round(time.Millisecond))
			} else {
				t.Errorf("%s %q: returned %d hits for a query the corpus cannot answer; top hit is %s of %q",
					tc.ID, tc.Query, len(result.Hits),
					result.Hits[0].ArticleNo, result.Hits[0].DocumentTitle)
			}
			continue
		}

		rank := findExpected(result.Hits, tc)
		switch {
		case rank == 0:
			t.Errorf("%s %q: none of articles %v appeared in the top %d. Got: %s",
				tc.ID, tc.Query, tc.ExpectArticles, successAtK, describeHits(result.Hits))
		default:
			tally.hits++
			t.Logf("PASS %s  rank %d  %v  matched=%v",
				tc.ID, rank, elapsed.Round(time.Millisecond), result.Hits[rank-1].MatchedBy)
		}

		if tc.ExpectRank > 0 {
			tally.rankCases++
			switch {
			case rank == 0:
				// Already reported above.
			case rank <= tc.ExpectRank:
				tally.rankPinned++
			default:
				t.Errorf("%s %q: a query naming an article is a lookup, not a ranking — wanted rank %d or better, got %d",
					tc.ID, tc.Query, tc.ExpectRank, rank)
			}
		}
	}

	t.Logf("")
	t.Logf("Explorer search — hybrid")
	t.Logf("  answerable cases      %d", tally.total-tally.emptyCases)
	t.Logf("  success@%d             %.0f%% (target %.0f%%)",
		successAtK, tally.successRate()*100, explorerSuccessTarget*100)
	t.Logf("  exact lookups at rank %d/%d", tally.rankPinned, tally.rankCases)
	t.Logf("  out-of-corpus correct %d/%d", tally.emptyCorrect, tally.emptyCases)
	t.Logf("  over %v               %d (worst %v)",
		searchLatencyBudget, tally.slow, tally.worstTime.Round(time.Millisecond))

	if got := tally.successRate(); got < explorerSuccessTarget {
		t.Errorf("success@%d is %.2f, below AC-002's %.2f", successAtK, got, explorerSuccessTarget)
	}
	if tally.emptyCorrect != tally.emptyCases {
		t.Errorf("out-of-corpus correctness %d/%d; returning something for an unanswerable query is the failure this exists to catch",
			tally.emptyCorrect, tally.emptyCases)
	}
}

// TestExplorerModeSplit measures keyword against semantic on the same cases.
//
// The point is not to pick a winner. It is to keep the claim that both arms are
// needed honest: if keyword alone scored the same, the embedding call in the
// search path would be latency and quota spent for nothing, and if semantic
// alone scored the same, the lexical arm would be.
func TestExplorerModeSplit(t *testing.T) {
	dataset, err := LoadExplorerDataset()
	if err != nil {
		t.Fatalf("explorer dataset: %v", err)
	}
	catalogue := buildCatalogue(t)

	modes := []documents.SearchMode{
		documents.SearchModeKeyword,
		documents.SearchModeSemantic,
		documents.SearchModeHybrid,
	}

	type score struct{ all, semanticOnly int }
	scores := map[documents.SearchMode]score{}
	var answerable, semanticCases int

	for _, mode := range modes {
		var s score
		answerable, semanticCases = 0, 0

		for _, tc := range dataset.Cases {
			if tc.ExpectEmpty || tc.Adjacent {
				continue
			}
			answerable++
			if tc.Semantic {
				semanticCases++
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			result, err := catalogue.Search(ctx, documents.SearchFilter{
				Term:  tc.Query,
				Mode:  mode,
				Limit: successAtK,
			})
			cancel()
			if err != nil {
				t.Errorf("%s in %s mode: %v", tc.ID, mode, err)
				continue
			}

			if findExpected(result.Hits, tc) > 0 {
				s.all++
				if tc.Semantic {
					s.semanticOnly++
				}
			}
		}
		scores[mode] = s
	}

	t.Logf("")
	t.Logf("Explorer search — by mode (success@%d)", successAtK)
	t.Logf("  %-9s  %-18s  %s", "mode", "all cases", "plain-language cases")
	for _, mode := range modes {
		s := scores[mode]
		t.Logf("  %-9s  %2d/%2d (%3.0f%%)       %2d/%2d (%3.0f%%)",
			mode,
			s.all, answerable, pct(s.all, answerable),
			s.semanticOnly, semanticCases, pct(s.semanticOnly, semanticCases))
	}

	// Hybrid must not be worse than either arm alone. If it is, fusion is
	// actively destroying a signal rather than combining two.
	hybrid := scores[documents.SearchModeHybrid].all
	for _, mode := range []documents.SearchMode{documents.SearchModeKeyword, documents.SearchModeSemantic} {
		if scores[mode].all > hybrid {
			t.Errorf("%s mode scored %d and hybrid scored %d: fusion is losing results, not combining them",
				mode, scores[mode].all, hybrid)
		}
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

// findExpected returns the 1-based rank of the first correct hit, or 0.
//
// A hit is correct only when the article number *and* the document both match:
// article numbers repeat across codes, so "Article 5" alone would score a hit
// on the Constitution for a Commercial Code case.
func findExpected(hits []documents.SearchHit, tc explorerCase) int {
	for i, hit := range hits {
		if !matchesAnyArticle(hit.ArticleNo, tc.ExpectArticles) {
			continue
		}
		if !matchesAnyDocument(hit.DocumentTitle, tc.ExpectDocuments) {
			continue
		}
		return i + 1
	}
	return 0
}

func matchesAnyArticle(articleNo string, expected []string) bool {
	for _, want := range expected {
		if articleNo == want {
			return true
		}
	}
	return false
}

func matchesAnyDocument(title string, expected []string) bool {
	// No expectation means any document counts.
	if len(expected) == 0 {
		return true
	}
	for _, want := range expected {
		if strings.Contains(strings.ToLower(title), strings.ToLower(want)) {
			return true
		}
	}
	return false
}

func describeHits(hits []documents.SearchHit) string {
	if len(hits) == 0 {
		return "(nothing)"
	}
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, hit.ArticleNo+" of "+shortTitle(hit.DocumentTitle))
	}
	return strings.Join(parts, ", ")
}

func shortTitle(title string) string {
	if len(title) <= 34 {
		return title
	}
	return title[:34] + "…"
}

// TestExplorerAdjacentTopics measures a limitation rather than asserting a
// target.
//
// These are real legal questions about legislation the corpus does not contain.
// Search still returns provisions for them, because their nearest chunks fall
// inside the same similarity band genuine hits occupy — so the semantic floor
// cannot reject them without rejecting real results too (see
// documents.vectorMinSimilarity).
//
// It never fails. A test that fails on a known gap is noise in every run until
// the gap closes; a test that prints the number makes the gap visible and makes
// it obvious when reranking finally moves it.
func TestExplorerAdjacentTopics(t *testing.T) {
	dataset, err := LoadExplorerDataset()
	if err != nil {
		t.Fatalf("explorer dataset: %v", err)
	}
	catalogue := buildCatalogue(t)

	var probes, returnedResults int
	for _, tc := range dataset.Cases {
		if !tc.Adjacent {
			continue
		}
		probes++

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result, err := catalogue.Search(ctx, documents.SearchFilter{
			Term:  tc.Query,
			Mode:  documents.SearchModeHybrid,
			Limit: successAtK,
		})
		cancel()
		if err != nil {
			t.Errorf("%s: search failed: %v", tc.ID, err)
			continue
		}

		if len(result.Hits) == 0 {
			t.Logf("rejected %s  %q", tc.ID, tc.Query)
			continue
		}
		returnedResults++
		t.Logf("returned %s  %q -> %s", tc.ID, tc.Query, describeHits(result.Hits))
	}

	t.Logf("")
	t.Logf("Adjacent legal topics not in the corpus")
	t.Logf("  probes                     %d", probes)
	t.Logf("  still returned provisions  %d/%d", returnedResults, probes)
	t.Logf("")
	t.Logf("  This is the known gap, not a regression. Closing it needs a")
	t.Logf("  reranker judging relevance on this path, which the search budget")
	t.Logf("  (< 1s, SRS 14.1) does not currently allow.")
}
