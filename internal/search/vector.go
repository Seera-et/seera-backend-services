package search

import (
	"context"
	"fmt"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// vectorSearch finds chunks by embedding similarity.
//
// This is the arm that answers a question phrased in a citizen's words with a
// provision phrased in a drafter's. It is also the arm most exposed to the
// Amharic embedding risk: the model is not evaluated for Amharic, and if the
// language-split Precision@5 gate fails, this is what gets swapped.
func (s *Service) vectorSearch(ctx context.Context, q Query) ([]Result, error) {
	// EmbedQuery, not Embed: providers that distinguish a question from a passage
	// embed them into different spaces, and using the document path for a query
	// costs retrieval accuracy for nothing.
	vector, err := s.embedder.EmbedQuery(ctx, q.Text)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vector) != s.embedder.Dimensions() {
		return nil, fmt.Errorf("embedder returned %d dimensions, want %d",
			len(vector), s.embedder.Dimensions())
	}

	// <=> is cosine distance in pgvector, so smaller is nearer. Score is
	// reported as similarity (1 - distance) because every other arm reports
	// larger-is-better, and a mixed convention would eventually be fused wrong.
	//
	// The embedding table is resolved at construction from the allowlist in
	// internal/legal, never from a caller.
	query := fmt.Sprintf(`
		SELECT%s, 1 - (e.embedding <=> $1::vector) AS score%s
		JOIN %s e ON e.chunk_id = c.id
		WHERE v.status = 'published'
		  AND (v.effective_from IS NULL OR v.effective_from <= $2)
		  AND (v.repealed_at IS NULL OR v.repealed_at > $2)
		  AND ($3 = '' OR c.language = $3)
		ORDER BY e.embedding <=> $1::vector
		LIMIT $4`, selectColumns, fromChunks, s.table)

	rows, err := s.pool.Query(ctx, query,
		legal.VectorLiteral(vector), q.AsOf, string(q.Language), q.PerRetriever)
	if err != nil {
		return nil, err
	}
	return scanResults(rows, RetrieverVector)
}
