package documents

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

func query(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

func TestParseSearchFilterDefaults(t *testing.T) {
	f, err := ParseSearchFilter(query("q", "  capital requirements  "))
	if err != nil {
		t.Fatalf("ParseSearchFilter: %v", err)
	}

	if f.Term != "capital requirements" {
		t.Errorf("Term = %q, want it trimmed", f.Term)
	}
	if f.Mode != SearchModeHybrid {
		t.Errorf("Mode = %q, want hybrid by default", f.Mode)
	}
	if f.Limit != defaultSearchPageSize {
		t.Errorf("Limit = %d, want %d", f.Limit, defaultSearchPageSize)
	}
	// The default must not quietly include repealed law.
	if f.IncludeSuperseded {
		t.Error("IncludeSuperseded is true by default; superseded law must be opt-in")
	}
}

func TestParseSearchFilterRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		q    url.Values
	}{
		{"no term", query()},
		{"blank term", query("q", "   ")},
		{"unknown mode", query("q", "x", "mode", "magic")},
		{"unknown language", query("q", "x", "language", "fr")},
		{"unknown type", query("q", "x", "type", "memo")},
		{"year out of range", query("q", "x", "year", "1700")},
		{"year not a number", query("q", "x", "year", "recent")},
		{"limit over cap", query("q", "x", "limit", "500")},
		{"limit zero", query("q", "x", "limit", "0")},
		{"non-boolean include_superseded", query("q", "x", "include_superseded", "sometimes")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseSearchFilter(tt.q); err == nil {
				t.Fatal("want an error: a filter that silently does nothing reads as a corpus with holes in it")
			}
		})
	}
}

func TestParseSearchFilterAcceptsEveryFilter(t *testing.T) {
	f, err := ParseSearchFilter(query(
		"q", "capital",
		"mode", "semantic",
		"language", "am",
		"type", "code",
		"domain", "Commercial Law",
		"year", "2021",
		"document", "doc-1",
		"include_superseded", "true",
		"limit", "5",
		"offset", "10",
	))
	if err != nil {
		t.Fatalf("ParseSearchFilter: %v", err)
	}

	want := SearchFilter{
		Term:              "capital",
		Mode:              SearchModeSemantic,
		Language:          legal.LanguageAmharic,
		DocType:           legal.DocTypeCode,
		Domain:            "Commercial Law",
		Year:              2021,
		DocumentID:        "doc-1",
		IncludeSuperseded: true,
		Limit:             5,
		Offset:            10,
	}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("filter =\n%+v\nwant\n%+v", f, want)
	}
}

func TestArticleNumbersIn(t *testing.T) {
	tests := []struct {
		name string
		term string
		want []string
	}{
		{"plain reference", "what does article 45 say", []string{"45"}},
		{"abbreviated", "Art. 45(2)", []string{"45"}},
		{"enumeration", "articles 45, 46 and 47", []string{"45", "46", "47"}},
		{"amharic reference", "አንቀጽ ፵፭", []string{"45"}},
		{"bare arabic number", "45", []string{"45"}},
		{"bare geez number", "፵፭", []string{"45"}},
		{"bare number with padding", "  45  ", []string{"45"}},
		{"ordinary words", "capital requirements", nil},
		{"number inside a phrase is not a reference", "45 day notice period", nil},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := articleNumbersIn(tt.term)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("articleNumbersIn(%q) = %v, want %v", tt.term, got, tt.want)
			}
		})
	}
}

func TestFuseRewardsAgreementBetweenArms(t *testing.T) {
	// "b" is second in both arms; "a" and "c" are first in one arm each. RRF
	// gives the corroborated result the higher total, which is the whole reason
	// hybrid search beats either arm alone.
	arms := []arm{
		{name: "keyword", hits: []candidate{
			{ArticleID: "a", ChunkID: "a1"},
			{ArticleID: "b", ChunkID: "b1"},
		}},
		{name: "vector", hits: []candidate{
			{ArticleID: "c", ChunkID: "c1"},
			{ArticleID: "b", ChunkID: "b2"},
		}},
	}

	got := fuse(arms, maxPool)
	if len(got) != 3 {
		t.Fatalf("fused %d articles, want 3", len(got))
	}
	if got[0].articleID != "b" {
		t.Errorf("top result is %q, want \"b\" — the article both arms found", got[0].articleID)
	}
	if want := []string{"keyword", "vector"}; !reflect.DeepEqual(got[0].matchedBy, want) {
		t.Errorf("matchedBy = %v, want %v", got[0].matchedBy, want)
	}

	// The chunk carried forward is the one from the arm that saw the article
	// first, so a hit always resolves to a real chunk of that article.
	if got[0].chunkID != "b1" {
		t.Errorf("chunkID = %q, want \"b1\"", got[0].chunkID)
	}
}

func TestFuseCollapsesAnArticleFoundTwiceByOneArm(t *testing.T) {
	// An arm should already have been reduced to one entry per article, but if
	// it were not, the fused list must still list the article once — a reader
	// seeing Article 45 three times reads three findings where there is one.
	arms := []arm{{name: "keyword", hits: []candidate{
		{ArticleID: "a", ChunkID: "a1"},
		{ArticleID: "a", ChunkID: "a2"},
	}}}

	got := fuse(arms, maxPool)
	if len(got) != 1 {
		t.Fatalf("fused %d results, want 1", len(got))
	}
	if want := []string{"keyword"}; !reflect.DeepEqual(got[0].matchedBy, want) {
		t.Errorf("matchedBy = %v, want %v — one arm, named once", got[0].matchedBy, want)
	}
}

func TestFuseRespectsLimit(t *testing.T) {
	hits := make([]candidate, 0, 10)
	for i := range 10 {
		hits = append(hits, candidate{ArticleID: legal.ArticleID(string(rune('a' + i)))})
	}

	got := fuse([]arm{{name: "keyword", hits: hits}}, 4)
	if len(got) != 4 {
		t.Fatalf("fused %d results, want the limit of 4", len(got))
	}
	if got[0].articleID != "a" {
		t.Errorf("top result is %q, want the arm's own best, \"a\"", got[0].articleID)
	}
}

func TestFuseOfNothingIsEmpty(t *testing.T) {
	if got := fuse(nil, maxPool); len(got) != 0 {
		t.Errorf("fuse(nil) returned %d results, want none", len(got))
	}
}

// ---------------------------------------------------------------------------
// Service.Search
// ---------------------------------------------------------------------------

// searchStore builds a fake whose pool holds n articles, all hydratable.
func searchStore(n int, capped bool, mode SearchMode) *fakeStore {
	pool := Pool{Capped: capped, Mode: mode}
	hydrated := map[legal.ArticleID]SearchHit{}
	for i := range n {
		id := legal.ArticleID(string(rune('a' + i)))
		pool.Ranked = append(pool.Ranked, ranked{articleID: id, score: float64(n - i)})
		hydrated[id] = SearchHit{ArticleID: id}
	}
	return &fakeStore{pool: pool, hydrated: hydrated}
}

func TestSearchPagesInsideTheRankedPool(t *testing.T) {
	store := searchStore(10, false, SearchModeHybrid)
	svc := NewService(store)

	result, err := svc.Search(context.Background(),
		SearchFilter{Term: "capital", Limit: 4, Offset: 4})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(result.Hits) != 4 {
		t.Fatalf("got %d hits, want 4", len(result.Hits))
	}
	// Total is the pool, not the page: it is what the reader can page through.
	if result.Total != 10 {
		t.Errorf("Total = %d, want the pool size 10", result.Total)
	}
	if result.Hits[0].ArticleID != "e" {
		t.Errorf("first hit is %q, want \"e\" — the fifth ranked article", result.Hits[0].ArticleID)
	}
	// Only the page is hydrated. Loading text for all ten would be work thrown
	// away, and this is the assertion that keeps it that way.
	if len(store.lastPage) != 4 {
		t.Errorf("hydrated %d rows, want only the page's 4", len(store.lastPage))
	}
}

func TestSearchOffsetPastTheEndIsAnEmptyPage(t *testing.T) {
	store := searchStore(3, false, SearchModeHybrid)
	svc := NewService(store)

	result, err := svc.Search(context.Background(),
		SearchFilter{Term: "capital", Limit: 10, Offset: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Hits) != 0 {
		t.Errorf("got %d hits, want none", len(result.Hits))
	}
	if result.Hits == nil {
		t.Error("Hits is nil; it must marshal as [] rather than null")
	}
	if result.Total != 3 {
		t.Errorf("Total = %d, want 3 so the client can page back", result.Total)
	}
	if store.hydrateHt != 0 {
		t.Error("hydrated an empty page; the store should not have been asked")
	}
}

func TestSearchLastPageIsShort(t *testing.T) {
	svc := NewService(searchStore(5, false, SearchModeHybrid))

	result, err := svc.Search(context.Background(),
		SearchFilter{Term: "capital", Limit: 4, Offset: 4})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("got %d hits, want the last 1", len(result.Hits))
	}
}

func TestSearchReportsTheModeActuallyUsed(t *testing.T) {
	// The reader asked for hybrid; the deployment has no embedder, so ranking
	// reports keyword. The response must say keyword, not repeat the request.
	svc := NewService(searchStore(2, false, SearchModeKeyword))

	result, err := svc.Search(context.Background(),
		SearchFilter{Term: "capital", Mode: SearchModeHybrid, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.Mode != SearchModeKeyword {
		t.Errorf("Mode = %q, want keyword — the mode that ran", result.Mode)
	}
}

func TestSearchCarriesCappedAndArticleRefs(t *testing.T) {
	store := searchStore(2, true, SearchModeHybrid)
	svc := NewService(store)

	result, err := svc.Search(context.Background(),
		SearchFilter{Term: "article 45", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if !result.Capped {
		t.Error("Capped = false; an exact total that is really a truncation point misstates the corpus")
	}

	want := []string{"45"}
	if !reflect.DeepEqual(result.ArticleRefs, want) {
		t.Errorf("ArticleRefs = %v, want %v", result.ArticleRefs, want)
	}
	// The numbers the service parsed must be the ones ranking was given.
	if !reflect.DeepEqual(store.lastNums, want) {
		t.Errorf("ranking got numbers %v, want %v", store.lastNums, want)
	}
}

func TestSearchPropagatesRankingFailure(t *testing.T) {
	sentinel := errors.New("embedding provider unavailable")
	svc := NewService(&fakeStore{poolErr: sentinel})

	if _, err := svc.Search(context.Background(),
		SearchFilter{Term: "capital", Limit: 10}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the ranking failure to surface", err)
	}
}
