package documents

import (
	"context"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Store is the persistence this service needs.
type Store interface {
	List(ctx context.Context, f ListFilter) (ListResult, error)
	Get(ctx context.Context, id legal.DocumentID) (Detail, error)
	ResolveVersion(ctx context.Context, documentID legal.DocumentID, versionID legal.VersionID) (VersionSummary, error)
	Articles(ctx context.Context, versionID legal.VersionID, after, limit int) ([]Article, error)
	Stats(ctx context.Context) (Stats, error)
}

// Service serves the catalogue.
//
// Thin by design: the visibility rule is SQL in the repository, and pagination
// bounds are validation in the DTOs. What is left is assembling a page and
// deciding where it ends.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

func (s *Service) List(ctx context.Context, f ListFilter) (ListResult, error) {
	return s.store.List(ctx, f)
}

func (s *Service) Get(ctx context.Context, id legal.DocumentID) (Detail, error) {
	return s.store.Get(ctx, id)
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	return s.store.Stats(ctx)
}

// Articles returns a page of one version's articles.
func (s *Service) Articles(
	ctx context.Context,
	documentID legal.DocumentID,
	f ArticleFilter,
) (ArticlePage, error) {
	version, err := s.store.ResolveVersion(ctx, documentID, f.VersionID)
	if err != nil {
		return ArticlePage{}, err
	}

	// One more than asked for: if it comes back, there is another page, and the
	// extra row is dropped. Cheaper and more reliable than comparing against a
	// total that another ingestion could have changed between queries.
	articles, err := s.store.Articles(ctx, version.ID, f.After, f.Limit+1)
	if err != nil {
		return ArticlePage{}, err
	}

	page := ArticlePage{
		Version:  version,
		Articles: articles,
		Total:    version.ArticleCount,
	}

	if len(articles) > f.Limit {
		page.Articles = articles[:f.Limit]
		next := page.Articles[len(page.Articles)-1].Ordinal
		page.NextAfter = &next
	}

	return page, nil
}
