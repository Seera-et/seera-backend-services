package search

import (
	"context"
	"sort"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// trigramThreshold is the minimum pg_trgm similarity for a fallback match.
// Below this the results are noise.
const trigramThreshold = 0.2

// keywordSearch finds chunks by lexical match.
//
// It runs once per language rather than once overall. The full-text index is
// built per row under the configuration for that row's language, and a tsquery
// only matches lexemes produced by the same configuration — so one query cannot
// correctly span both. Two indexed queries are also faster than one that
// computes the configuration per row, which would defeat the GIN index entirely.
func (s *Service) keywordSearch(ctx context.Context, q Query) ([]Result, error) {
	languages := []legal.Language{legal.LanguageEnglish, legal.LanguageAmharic}
	if q.Language != "" {
		languages = []legal.Language{q.Language}
	}

	var all []Result
	for _, lang := range languages {
		results, err := s.keywordSearchLanguage(ctx, q, lang)
		if err != nil {
			return nil, err
		}

		// Amharic indexes under the 'simple' configuration — Postgres has no
		// Amharic dictionary, so there is no stemming and no stop-word removal,
		// and a query only matches text that shares whole word forms. Ethiopian
		// legal Amharic is heavily inflected, so an exact-form miss is common;
		// trigram similarity recovers the near-misses.
		if lang == legal.LanguageAmharic && len(results) == 0 {
			results, err = s.trigramSearch(ctx, q, lang)
			if err != nil {
				return nil, err
			}
		}
		all = append(all, results...)
	}

	// Scores from ts_rank_cd are comparable across languages closely enough to
	// order one candidate list; fusion re-ranks by position anyway.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > q.PerRetriever {
		all = all[:q.PerRetriever]
	}
	return all, nil
}

func (s *Service) keywordSearchLanguage(ctx context.Context, q Query, lang legal.Language) ([]Result, error) {
	// websearch_to_tsquery accepts what users actually type — quoted phrases,
	// OR, leading minus — without erroring on syntax the way to_tsquery does.
	//
	// The configuration is passed as a parameter and must match the one the
	// generated fts column was built with for this language; legal.FTSConfig is
	// the single source of that mapping on the Go side.
	const query = `
		SELECT` + selectColumns + `, ts_rank_cd(c.fts, tsq) AS score` + fromChunks + `,
		     websearch_to_tsquery($1::regconfig, $2) AS tsq
		WHERE c.fts @@ tsq
		  AND c.language = $3
		  AND v.status = 'published'
		  AND (v.effective_from IS NULL OR v.effective_from <= $4)
		  AND (v.repealed_at IS NULL OR v.repealed_at > $4)
		ORDER BY score DESC
		LIMIT $5`

	rows, err := s.pool.Query(ctx, query,
		lang.FTSConfig(), q.Text, string(lang), q.AsOf, q.PerRetriever)
	if err != nil {
		return nil, err
	}
	return scanResults(rows, RetrieverKeyword)
}

// trigramSearch is the fuzzy fallback for languages Postgres cannot stem.
func (s *Service) trigramSearch(ctx context.Context, q Query, lang legal.Language) ([]Result, error) {
	const query = `
		SELECT` + selectColumns + `, similarity(c.text, $1) AS score` + fromChunks + `
		WHERE c.language = $2
		  AND similarity(c.text, $1) > $3
		  AND v.status = 'published'
		  AND (v.effective_from IS NULL OR v.effective_from <= $4)
		  AND (v.repealed_at IS NULL OR v.repealed_at > $4)
		ORDER BY score DESC
		LIMIT $5`

	rows, err := s.pool.Query(ctx, query,
		q.Text, string(lang), trigramThreshold, q.AsOf, q.PerRetriever)
	if err != nil {
		return nil, err
	}
	return scanResults(rows, RetrieverTrigram)
}
