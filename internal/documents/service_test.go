package documents

import (
	"context"
	"errors"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// fakeStore is enough of the catalogue to test paging without Postgres. The SQL
// itself is exercised by the repository tests against a real database.
type fakeStore struct {
	version  VersionSummary
	articles []Article

	// lastLimit records what the service actually asked the store for.
	lastLimit int
	lastAfter int

	versionErr error

	// Search side. pool is what ranking returns; hydrated is what the store
	// would load for a page, keyed by article id.
	pool      Pool
	hydrated  map[legal.ArticleID]SearchHit
	poolErr   error
	lastPage  []ranked
	lastTerm  string
	lastNums  []string
	hydrateHt int
}

func (f *fakeStore) List(context.Context, ListFilter) (ListResult, error) {
	return ListResult{}, nil
}

func (f *fakeStore) Get(context.Context, legal.DocumentID) (Detail, error) {
	return Detail{}, nil
}

func (f *fakeStore) Stats(context.Context) (Stats, error) { return Stats{}, nil }

func (f *fakeStore) ResolveVersion(context.Context, legal.DocumentID, legal.VersionID) (VersionSummary, error) {
	if f.versionErr != nil {
		return VersionSummary{}, f.versionErr
	}
	return f.version, nil
}

func (f *fakeStore) Articles(_ context.Context, _ legal.VersionID, after, limit int) ([]Article, error) {
	f.lastAfter, f.lastLimit = after, limit

	out := []Article{}
	for _, a := range f.articles {
		if a.Ordinal <= after {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeStore) SearchPool(_ context.Context, filter SearchFilter, numbers []string) (Pool, error) {
	f.lastTerm, f.lastNums = filter.Term, numbers
	if f.poolErr != nil {
		return Pool{}, f.poolErr
	}
	return f.pool, nil
}

func (f *fakeStore) Hydrate(_ context.Context, _ SearchFilter, page []ranked) ([]SearchHit, error) {
	f.lastPage = page
	f.hydrateHt++

	out := make([]SearchHit, 0, len(page))
	for _, p := range page {
		hit, ok := f.hydrated[p.articleID]
		if !ok {
			continue
		}
		out = append(out, hit)
	}
	return out, nil
}

func articles(n int) []Article {
	out := make([]Article, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Article{Ordinal: i})
	}
	return out
}

func TestServiceArticlesPaginates(t *testing.T) {
	store := &fakeStore{
		version:  VersionSummary{ID: "v1", ArticleCount: 10},
		articles: articles(10),
	}
	svc := NewService(store)

	page, err := svc.Articles(context.Background(), "d1", ArticleFilter{Limit: 4})
	if err != nil {
		t.Fatalf("Articles: %v", err)
	}

	if got, want := len(page.Articles), 4; got != want {
		t.Fatalf("got %d articles, want %d", got, want)
	}
	// One more than the page size is fetched to detect the next page, and the
	// extra row must not be served.
	if store.lastLimit != 5 {
		t.Errorf("store asked for limit %d, want 5", store.lastLimit)
	}
	if page.NextAfter == nil {
		t.Fatal("NextAfter is nil, want a cursor")
	}
	if *page.NextAfter != 4 {
		t.Errorf("NextAfter = %d, want 4", *page.NextAfter)
	}
	if page.Total != 10 {
		t.Errorf("Total = %d, want 10", page.Total)
	}
}

func TestServiceArticlesLastPageHasNoCursor(t *testing.T) {
	store := &fakeStore{
		version:  VersionSummary{ID: "v1", ArticleCount: 3},
		articles: articles(3),
	}
	svc := NewService(store)

	page, err := svc.Articles(context.Background(), "d1", ArticleFilter{Limit: 10})
	if err != nil {
		t.Fatalf("Articles: %v", err)
	}
	if len(page.Articles) != 3 {
		t.Fatalf("got %d articles, want 3", len(page.Articles))
	}
	if page.NextAfter != nil {
		t.Errorf("NextAfter = %d, want nil at the end of the version", *page.NextAfter)
	}
}

func TestServiceArticlesContinuesFromCursor(t *testing.T) {
	store := &fakeStore{
		version:  VersionSummary{ID: "v1", ArticleCount: 10},
		articles: articles(10),
	}
	svc := NewService(store)

	page, err := svc.Articles(context.Background(), "d1", ArticleFilter{After: 8, Limit: 4})
	if err != nil {
		t.Fatalf("Articles: %v", err)
	}
	if store.lastAfter != 8 {
		t.Errorf("store asked after %d, want 8", store.lastAfter)
	}
	if len(page.Articles) != 2 {
		t.Fatalf("got %d articles, want the last 2", len(page.Articles))
	}
	if page.NextAfter != nil {
		t.Error("NextAfter should be nil once the version is exhausted")
	}
}

func TestServiceArticlesPropagatesNotFound(t *testing.T) {
	store := &fakeStore{versionErr: ErrNotFound}
	svc := NewService(store)

	_, err := svc.Articles(context.Background(), "d1", ArticleFilter{Limit: 10})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound so the handler can answer 404", err)
	}
}
