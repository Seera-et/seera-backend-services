package search

import (
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

func results(retriever Retriever, ids ...string) []Result {
	out := make([]Result, len(ids))
	for i, id := range ids {
		out[i] = Result{ChunkID: legal.ChunkID(id), Retriever: retriever}
	}
	return out
}

func ids(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r.ChunkID)
	}
	return out
}

// Agreement between arms is the signal hybrid search exists to capture: a chunk
// both lexical and semantic retrieval like should beat one that only a single
// arm ranks first.
func TestFuseRRFPromotesChunksFoundBySeveralArms(t *testing.T) {
	vector := results(RetrieverVector, "a", "b", "c")
	keyword := results(RetrieverKeyword, "d", "b", "e")

	fused := FuseRRF(vector, keyword)

	if got := ids(fused)[0]; got != "b" {
		t.Errorf("top result = %q, want %q — found by both arms", got, "b")
	}
	if len(fused) != 5 {
		t.Errorf("got %d results, want 5 distinct chunks", len(fused))
	}
}

func TestFuseRRFMarksMultiArmResultsAsHybrid(t *testing.T) {
	fused := FuseRRF(results(RetrieverVector, "a", "b"), results(RetrieverKeyword, "b"))

	byID := map[string]Result{}
	for _, r := range fused {
		byID[string(r.ChunkID)] = r
	}

	if got := byID["b"].Retriever; got != "hybrid" {
		t.Errorf("chunk found by two arms is labelled %q, want hybrid", got)
	}
	if got := byID["a"].Retriever; got != RetrieverVector {
		t.Errorf("chunk found by one arm is labelled %q, want vector", got)
	}
}

// The exact-article arm is fused at rank 1, which is the whole point of having
// it: a query that names a provision is not ambiguous about what it wants.
func TestFuseRRFRanksExactArticleMatchesFirst(t *testing.T) {
	exact := results(RetrieverArticle, "article-45")
	vector := results(RetrieverVector, "vague-1", "vague-2", "vague-3")

	fused := FuseRRF(exact, vector)

	if got := ids(fused)[0]; got != "article-45" {
		t.Errorf("top result = %q, want the exact article match", got)
	}
}

func TestFuseRRFScoresDecreaseWithRank(t *testing.T) {
	fused := FuseRRF(results(RetrieverVector, "a", "b", "c"))

	for i := 1; i < len(fused); i++ {
		if fused[i].Score > fused[i-1].Score {
			t.Fatalf("score at rank %d (%f) exceeds rank %d (%f)",
				i, fused[i].Score, i-1, fused[i-1].Score)
		}
	}

	want := 1.0 / (RRFConstant + 1)
	if got := fused[0].Score; got != want {
		t.Errorf("top score = %f, want %f", got, want)
	}
}

func TestFuseRRFHandlesEmptyInput(t *testing.T) {
	if got := FuseRRF(); len(got) != 0 {
		t.Errorf("FuseRRF() with no lists returned %d results", len(got))
	}
	if got := FuseRRF(nil, results(RetrieverVector, "a")); len(got) != 1 {
		t.Errorf("FuseRRF() ignored a populated list alongside an empty one: %d results", len(got))
	}
}

// Fusion must preserve the metadata that becomes the citation. Losing it here
// would produce an answer nobody can verify.
func TestFuseRRFPreservesResultMetadata(t *testing.T) {
	original := Result{
		ChunkID:       "chunk-1",
		ArticleID:     "article-1",
		VersionID:     "version-1",
		DocumentID:    "doc-1",
		DocumentTitle: "Commercial Code",
		ArticleNo:     "45",
		SectionPath:   []string{"Book II"},
		Language:      legal.LanguageEnglish,
		Text:          "A private limited company...",
		Retriever:     RetrieverVector,
	}

	fused := FuseRRF([]Result{original})
	if len(fused) != 1 {
		t.Fatalf("got %d results, want 1", len(fused))
	}

	got := fused[0]
	if got.ArticleID != original.ArticleID || got.VersionID != original.VersionID ||
		got.DocumentTitle != original.DocumentTitle || got.ArticleNo != original.ArticleNo ||
		got.Text != original.Text || got.Language != original.Language {
		t.Errorf("fusion dropped metadata:\n got %+v\nwant %+v", got, original)
	}
}
