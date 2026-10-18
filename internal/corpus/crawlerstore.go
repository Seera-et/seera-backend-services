package corpus

import (
	"github.com/Seera-et/seera-backend-services/internal/crawler"
)

// crawlerStore adapts a crawler store to Source.
//
// This is the only file in the package that imports internal/crawler, which is
// the point — see the Source doc comment.
type crawlerStore struct{ store *crawler.Store }

// NewCrawlerSource opens the crawler store rooted at dir.
func NewCrawlerSource(dir string) (Source, error) {
	s, err := crawler.NewStore(dir)
	if err != nil {
		return nil, err
	}
	return crawlerStore{store: s}, nil
}

func (c crawlerStore) Records() ([]crawler.Record, error) { return c.store.ListRecords() }
func (c crawlerStore) StructuredPath(id string) string    { return c.store.StructuredPath(id) }
