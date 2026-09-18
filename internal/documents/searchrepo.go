package documents

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// armFetch is how many chunk rows an arm pulls before it is reduced to one
// entry per article.
//
// Two per requested candidate: chunking is one chunk per article by default and
// only long articles split, so a small factor is enough to keep perArm distinct
// articles after the reduction.
const armFetch = perArm * 2

// trigramThreshold is the minimum pg_trgm similarity worth returning.
//
// Same value as the answer path's retrieval (internal/search): below it the
// matches are noise, and a reader who sees noise at rank 1 stops trusting the
// ranking.
const trigramThreshold = 0.2

// vectorMinSimilarity is the cosine-similarity floor on the semantic arm.
//
// The lexical arms have a natural floor — a tsquery either matches or it does
// not, and trigram has trigramThreshold — but kNN has none: it returns the
// nearest chunks however far away they are. Without a floor, "how do I bake
// sourdough bread" returns five articles of the Commercial Code, which is not
// a weak result, it is a misleading one.
//
// 0.50 is chosen from the measured distribution over the ingested corpus
// (2026-09-18, gemini-embedding-001, 879 chunks; see
// tests/evaluation/dataset/explorer_v1.yaml):
//
//	in-corpus queries, top hit            0.640 – 0.715
//	off-topic queries, top hit            0.485 – 0.498
//
// The gap between those two bands is wide, so the floor is not a delicate
// judgement call — it sits well below the weakest genuine hit measured and
// above the strongest unrelated one.
//
// What it deliberately does NOT do: separate the corpus from *adjacent legal
// topics* it does not contain. A query about VAT tops out at 0.61 and one about
// traffic fines at 0.636, which overlaps the in-corpus band — so a threshold
// that excluded them would also exclude real hits. Telling "no tax law is
// indexed" apart from "here is the tax law" needs a reranker judging relevance,
// not a distance. That gap is measured rather than papered over: see
// TestExplorerAdjacentTopics.
const vectorMinSimilarity = 0.50

// searchArgs collects bound values and hands back the placeholder for each.
//
// Filters are dynamic — a reader picks whichever of six they care about — so
// the WHERE clause is assembled rather than fixed. Values only ever reach the
// database through this, so no filter can become string-concatenated SQL by
// accident.
type searchArgs struct{ values []any }

func (a *searchArgs) add(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

// visibility renders the version-status predicate.
//
// Draft is never visible, exactly as in visibleVersions and exactly as in
// retrieval. Superseded is opt-in: see SearchFilter.IncludeSuperseded.
func visibility(f SearchFilter) string {
	if f.IncludeSuperseded {
		return `v.status IN ('published', 'superseded')`
	}
	return `v.status = 'published'`
}

// searchFilterSQL renders every filter except language, which each arm binds
// itself — the keyword arm has to partition by language because the full-text
// index is built per language, so language cannot be a shared predicate.
func searchFilterSQL(f SearchFilter, a *searchArgs) string {
	var b strings.Builder

	b.WriteString(" AND ")
	b.WriteString(visibility(f))

	if f.DocType != "" {
		b.WriteString(" AND d.doc_type = " + a.add(string(f.DocType)))
	}
	if f.Domain != "" {
		b.WriteString(" AND d.legal_domain = " + a.add(f.Domain))
	}
	if f.DocumentID != "" {
		b.WriteString(" AND c.document_id = " + a.add(string(f.DocumentID)) + "::uuid")
	}
	if f.Year != 0 {
		// Either date, because the corpus has documents that record only one of
		// the two and a year filter that matched just one of them would hide
		// half the catalogue for no reason the reader could see.
		p := a.add(f.Year)
		b.WriteString(" AND (extract(year FROM d.publication_date) = " + p +
			" OR extract(year FROM d.effective_date) = " + p + ")")
	}

	return b.String()
}

// searchLanguages is which languages an arm should run over.
func searchLanguages(f SearchFilter) []legal.Language {
	if f.Language != "" {
		return []legal.Language{f.Language}
	}
	return []legal.Language{legal.LanguageEnglish, legal.LanguageAmharic}
}

// fromChunksJoined is the join every arm shares. Articles are not joined here:
// the ranking phase only needs identifiers, and pulling article text for
// candidates that fusion will discard is work thrown away.
const fromChunksJoined = `
	FROM chunks c
	JOIN document_versions v ON v.id = c.version_id
	JOIN legal_documents d   ON d.id = c.document_id`

// SearchPool ranks the corpus and returns the fused candidates.
//
// The arms run concurrently. They are independent queries against the same
// pool, the vector arm additionally waits on an embedding HTTP call, and the
// Explorer's budget is under a second (SRS §14.1) — running them in sequence
// spends that budget three times over. This is the mistake the answer path made
// and had measured back at it: "the three retrieval arms run sequentially, so
// each is a separate round trip" (docs/QA_FEATURE_PLAN.md §7).
func (r *Repository) SearchPool(
	ctx context.Context, f SearchFilter, articleNumbers []string,
) (Pool, error) {
	mode := r.effectiveMode(f.Mode)

	// Fixed slots rather than an append from several goroutines: the arm order
	// decides nothing about ranking — RRF is symmetric — but a deterministic
	// order keeps identical queries returning identical tie-breaks.
	arms := make([]arm, 3)
	group, ctx := errgroup.WithContext(ctx)

	if mode != SearchModeSemantic {
		group.Go(func() error {
			a, err := r.keywordArm(ctx, f)
			arms[0] = a
			return err
		})
	}
	if mode != SearchModeKeyword {
		group.Go(func() error {
			a, err := r.vectorArm(ctx, f)
			arms[1] = a
			return err
		})
	}
	// The article arm runs in every mode. A reader who typed "Article 45" has
	// named a provision, and that is a lookup — withholding it because they
	// picked "semantic" would be honouring the label over the request.
	group.Go(func() error {
		a, err := r.articleArm(ctx, f, articleNumbers)
		arms[2] = a
		return err
	})

	if err := group.Wait(); err != nil {
		return Pool{}, err
	}

	pool := Pool{Mode: mode}
	populated := make([]arm, 0, len(arms))
	for _, a := range arms {
		if len(a.hits) == 0 {
			continue
		}
		populated = append(populated, a)
		pool.Capped = pool.Capped || a.saturated
	}

	pool.Ranked = fuse(populated, maxPool)
	if len(pool.Ranked) == maxPool {
		// The pool itself was the limit, so there is more behind it regardless
		// of what any single arm reported.
		pool.Capped = true
	}
	return pool, nil
}

// effectiveMode narrows the requested mode to what this deployment can do.
//
// A hybrid or semantic request with no embedding provider is served as keyword
// search rather than refused. The corpus is browsable and lexically searchable
// with nothing but a database, and degrading loudly — the mode is reported back
// — beats failing a reader's search because an API key is absent.
func (r *Repository) effectiveMode(requested SearchMode) SearchMode {
	if r.embedder == nil {
		return SearchModeKeyword
	}
	return requested
}

// Each arm below is a separate method rather than one query with a mode switch,
// because they have genuinely different shapes: keyword partitions by language,
// trigram is a fallback, vector needs an embedding call and must keep its
// index-ordered output, and the article arm is a lookup rather than a search.

// keywordArm finds articles by lexical match, once per language.
//
// Once per language, not once overall: the fts column is generated under the
// configuration for that row's language, and a tsquery only matches lexemes
// produced by the same configuration — so a single query cannot correctly span
// both. Two indexed queries also beat one that computes the configuration per
// row, which would defeat the GIN index entirely.
func (r *Repository) keywordArm(ctx context.Context, f SearchFilter) (arm, error) {
	out := arm{name: string(search.RetrieverKeyword)}

	for _, lang := range searchLanguages(f) {
		hits, saturated, err := r.keywordArmLanguage(ctx, f, lang)
		if err != nil {
			return arm{}, err
		}

		// Amharic indexes under 'simple' — Postgres ships no Amharic stemmer or
		// stop words — so a query only matches text sharing whole word forms.
		// Ethiopian legal Amharic is heavily inflected, so an exact-form miss is
		// routine; trigram similarity recovers the near-misses. A fallback, not
		// a parallel arm: it runs only when full-text found nothing.
		if lang == legal.LanguageAmharic && len(hits) == 0 {
			hits, saturated, err = r.trigramArmLanguage(ctx, f, lang)
			if err != nil {
				return arm{}, err
			}
		}

		out.hits = append(out.hits, hits...)
		out.saturated = out.saturated || saturated
	}

	// Merged by score across languages. Each language's hits are ordered within
	// themselves but concatenated between, and concatenation would make every
	// English hit outrank every Amharic one regardless of how well it matched.
	// ts_rank_cd values from the two configurations are comparable closely
	// enough to order one candidate list, which is all this has to do — fusion
	// re-ranks by position afterwards.
	sort.SliceStable(out.hits, func(i, j int) bool {
		return out.hits[i].Score > out.hits[j].Score
	})
	if len(out.hits) > perArm {
		out.hits = out.hits[:perArm]
		out.saturated = true
	}
	return out, nil
}

func (r *Repository) keywordArmLanguage(
	ctx context.Context, f SearchFilter, lang legal.Language,
) ([]candidate, bool, error) {
	args := &searchArgs{}
	config := args.add(lang.FTSConfig())
	term := args.add(f.Term)
	language := args.add(string(lang))
	filters := searchFilterSQL(f, args)
	limit := args.add(armFetch)

	// websearch_to_tsquery accepts what readers actually type — quoted phrases,
	// OR, a leading minus — instead of erroring on syntax the way to_tsquery
	// does. The configuration is bound as a parameter and must match the one the
	// generated column was built under; legal.Language.FTSConfig is the single
	// source of that mapping on the Go side.
	query := `
		SELECT c.article_id, c.id, ts_rank_cd(c.fts, tsq) AS score` + fromChunksJoined + `,
		     websearch_to_tsquery(` + config + `::regconfig, ` + term + `) AS tsq
		WHERE c.fts @@ tsq
		  AND c.language = ` + language + filters + `
		ORDER BY score DESC, c.article_id
		LIMIT ` + limit

	return r.scanCandidates(ctx, query, args.values, armFetch)
}

// trigramArmLanguage is the fuzzy fallback for languages Postgres cannot stem.
func (r *Repository) trigramArmLanguage(
	ctx context.Context, f SearchFilter, lang legal.Language,
) ([]candidate, bool, error) {
	args := &searchArgs{}
	term := args.add(f.Term)
	language := args.add(string(lang))
	threshold := args.add(trigramThreshold)
	filters := searchFilterSQL(f, args)
	limit := args.add(armFetch)

	query := `
		SELECT c.article_id, c.id, similarity(c.text, ` + term + `) AS score` + fromChunksJoined + `
		WHERE c.language = ` + language + `
		  AND similarity(c.text, ` + term + `) > ` + threshold + filters + `
		ORDER BY score DESC, c.article_id
		LIMIT ` + limit

	return r.scanCandidates(ctx, query, args.values, armFetch)
}

// vectorArm finds articles by embedding similarity.
//
// This is the arm that matches a provision phrased by a drafter to a query
// phrased by a citizen. Its rows are returned in index order and reduced in Go
// rather than deduplicated in SQL: a DISTINCT ON would force a sort by
// article_id and silently stop the HNSW index from being used at all.
func (r *Repository) vectorArm(ctx context.Context, f SearchFilter) (arm, error) {
	if r.embedder == nil {
		return arm{}, nil
	}

	// EmbedQuery, not Embed: providers that separate questions from passages
	// embed them into different spaces, and using the document path for a query
	// costs accuracy for nothing.
	vector, err := r.embedder.EmbedQuery(ctx, f.Term)
	if err != nil {
		return arm{}, fmt.Errorf("embed search term: %w", err)
	}
	if len(vector) != r.embedder.Dimensions() {
		return arm{}, fmt.Errorf("embedder returned %d dimensions, want %d",
			len(vector), r.embedder.Dimensions())
	}

	args := &searchArgs{}
	embedding := args.add(legal.VectorLiteral(vector))
	language := args.add(string(f.Language))
	// The floor is converted to a distance here, in Go, and bound as one value.
	// Writing it as "< (1 - $n)" in SQL let Postgres infer the parameter's type
	// from the integer literal, which silently made the predicate match
	// everything — the floor existed and filtered nothing. Casting explicitly
	// would also work; binding the distance says what the number is.
	maxDistance := args.add(float64(1 - vectorMinSimilarity))
	filters := searchFilterSQL(f, args)
	limit := args.add(armFetch)

	// <=> is cosine distance, so nearer is smaller; the score is reported as
	// similarity so every arm agrees that larger is better. The embedding table
	// was resolved at construction from the allowlist in internal/legal, never
	// from a caller.
	//
	// The floor is a predicate on the distance expression the HNSW index orders
	// by, so it narrows the same scan rather than becoming a filter over every
	// row.
	query := `
		SELECT c.article_id, c.id, 1 - (e.embedding <=> ` + embedding + `::vector) AS score` +
		fromChunksJoined + `
		JOIN ` + r.embedTable + ` e ON e.chunk_id = c.id
		WHERE (` + language + ` = '' OR c.language = ` + language + `)
		  AND (e.embedding <=> ` + embedding + `::vector) < ` + maxDistance + `::float8` +
		filters + `
		ORDER BY e.embedding <=> ` + embedding + `::vector
		LIMIT ` + limit

	hits, saturated, err := r.scanCandidates(ctx, query, args.values, armFetch)
	if err != nil {
		return arm{}, err
	}
	return arm{name: string(search.RetrieverVector), hits: hits, saturated: saturated}, nil
}

// articleArm looks provisions up by number.
//
// The highest-leverage legal-specific behaviour in the Explorer, and the reason
// AC-002's "search by article number" case passes: neither a lexical index nor
// an embedding reliably returns Article 45 for "Article 45". To a tsquery the
// number is one low-information token, and to an embedding model "Article 45"
// and "Article 46" are nearly the same point. A direct lookup is exact, cheap,
// and precisely what was asked for.
func (r *Repository) articleArm(
	ctx context.Context, f SearchFilter, numbers []string,
) (arm, error) {
	if len(numbers) == 0 {
		return arm{}, nil
	}

	args := &searchArgs{}
	nums := args.add(numbers)
	language := args.add(string(f.Language))
	filters := searchFilterSQL(f, args)
	limit := args.add(armFetch)

	// Ordered by document then position rather than by a score: these are exact
	// matches, and their value comes from entering fusion at the top rank.
	query := `
		SELECT c.article_id, c.id, 1.0::float8 AS score` + fromChunksJoined + `
		JOIN articles a ON a.id = c.article_id
		WHERE a.article_no = ANY(` + nums + `)
		  AND (` + language + ` = '' OR c.language = ` + language + `)` + filters + `
		ORDER BY d.title, a.ordinal, c.ordinal
		LIMIT ` + limit

	hits, saturated, err := r.scanCandidates(ctx, query, args.values, armFetch)
	if err != nil {
		return arm{}, err
	}
	return arm{name: string(search.RetrieverArticle), hits: hits, saturated: saturated}, nil
}

// scanCandidates reads an arm's rows and reduces them to one chunk per article.
//
// The first chunk seen for an article wins, which — because every arm orders
// best-first — is that article's best-matching chunk. A reader browsing results
// wants one entry per provision; three chunks of Article 45 listed separately
// reads as three findings when it is one.
func (r *Repository) scanCandidates(
	ctx context.Context, query string, args []any, fetched int,
) ([]candidate, bool, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("search candidates: %w", err)
	}
	defer rows.Close()

	var (
		out  []candidate
		seen = map[legal.ArticleID]bool{}
		read int
	)
	for rows.Next() {
		var (
			articleID legal.ArticleID
			chunkID   legal.ChunkID
			score     float64
		)
		if err := rows.Scan(&articleID, &chunkID, &score); err != nil {
			return nil, false, fmt.Errorf("scan candidate: %w", err)
		}
		read++
		if seen[articleID] {
			continue
		}
		seen[articleID] = true
		out = append(out, candidate{ArticleID: articleID, ChunkID: chunkID, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("search candidates: %w", err)
	}

	// The arm filled its quota, so the corpus holds matches it did not rank.
	saturated := read >= fetched
	if len(out) > perArm {
		out = out[:perArm]
		saturated = true
	}
	return out, saturated, nil
}

// Hydrate loads the display rows for one page of fused results.
//
// Split from ranking on purpose: ranking touches identifiers only, and article
// text and snippets are computed for the twenty rows a reader is about to see
// rather than the two hundred that were ranked.
func (r *Repository) Hydrate(
	ctx context.Context, f SearchFilter, page []ranked,
) ([]SearchHit, error) {
	if len(page) == 0 {
		return []SearchHit{}, nil
	}

	ids := make([]string, len(page))
	for i, p := range page {
		ids[i] = string(p.articleID)
	}

	// Snippet options are bound rather than inlined so the markers stay in one
	// place. MaxFragments picks the matching passages instead of always
	// returning the head of the provision; the delimiter makes the elision
	// visible, because a snippet that silently joins two distant sentences
	// misrepresents what the article says.
	options := fmt.Sprintf(
		`StartSel=%q, StopSel=%q, MaxWords=60, MinWords=25, ShortWord=2, `+
			`MaxFragments=2, FragmentDelimiter=" … "`,
		HighlightStart, HighlightEnd)

	// The text-search configuration comes from the document's language, which is
	// the same mapping the generated fts column was built under. Taken from
	// legal_documents rather than from chunks so this stays one row per article:
	// joining chunks to pick up a language would multiply an article by its
	// chunk count.
	//
	// ts_headline does not use an index, so a function call per row costs nothing
	// at page size. Highlighting is best-effort by nature: a hit found only by
	// the vector arm has no lexical match to mark, and ts_headline then returns
	// the opening of the provision — the right fallback for a reader, and why the
	// snippet is never labelled "the matching sentence".
	const query = `
		SELECT
			d.id, d.title, d.doc_type, d.issuing_authority, d.language,
			v.id, v.version_label, v.status,
			a.id, a.article_no, a.title, a.chapter, a.section_path, a.ordinal,
			a.structure_confidence,
			ts_headline(cfg.config, a.text,
			            websearch_to_tsquery(cfg.config, $2), $3) AS snippet
		FROM articles a
		JOIN document_versions v ON v.id = a.version_id
		JOIN legal_documents d   ON d.id = a.document_id
		CROSS JOIN LATERAL (
			SELECT CASE WHEN d.language = 'en' THEN 'english'::regconfig
			            ELSE 'simple'::regconfig END AS config
		) cfg
		WHERE a.id = ANY($1::uuid[])
		ORDER BY array_position($1::uuid[], a.id)`

	rows, err := r.pool.Query(ctx, query, ids, f.Term, options)
	if err != nil {
		return nil, fmt.Errorf("hydrate search hits: %w", err)
	}
	defer rows.Close()

	byArticle := make(map[legal.ArticleID]SearchHit, len(page))
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(
			&h.DocumentID, &h.DocumentTitle, &h.DocType, &h.IssuingAuthority, &h.Language,
			&h.VersionID, &h.VersionLabel, &h.VersionStatus,
			&h.ArticleID, &h.ArticleNo, &h.ArticleTitle, &h.Chapter, &h.SectionPath,
			&h.Ordinal, &h.StructureConfidence, &h.Snippet,
		); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		byArticle[h.ArticleID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hydrate search hits: %w", err)
	}

	// Rebuilt from the ranked order rather than the row order, so the ranking is
	// the single source of sequence. An article that vanished between the two
	// queries — a version superseded mid-request — is dropped rather than
	// returned half-populated.
	hits := make([]SearchHit, 0, len(page))
	for _, p := range page {
		hit, ok := byArticle[p.articleID]
		if !ok {
			continue
		}
		hit.ChunkID = p.chunkID
		hit.Score = p.score
		hit.MatchedBy = p.matchedBy
		hits = append(hits, hit)
	}
	return hits, nil
}
