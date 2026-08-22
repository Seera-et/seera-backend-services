// Package search is hybrid retrieval over the legal corpus: vector similarity,
// lexical matching, and exact article lookup, fused into one ranked candidate
// list.
//
// It answers "which passages of Ethiopian law are relevant to this question".
// It does not decide what to do with them — no prompt construction, no
// generation. That is internal/rag.
package search

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Retriever names which strategy produced a result. Kept on every result
// because when retrieval is wrong, the first question is always "which arm
// found this, and which one should have".
type Retriever string

const (
	RetrieverVector  Retriever = "vector"
	RetrieverKeyword Retriever = "keyword"
	RetrieverTrigram Retriever = "trigram"
	RetrieverArticle Retriever = "article"
)

// Result is one retrieved chunk with the metadata that follows it all the way
// to the citation.
//
// None of these fields are optional decoration. The generator sees them, the
// response carries them back, and the UI renders a citation from them. Dropping
// metadata mid-pipeline is how a grounded answer turns into an unverifiable one.
type Result struct {
	ChunkID    legal.ChunkID
	ArticleID  legal.ArticleID
	VersionID  legal.VersionID
	DocumentID legal.DocumentID

	DocumentTitle    string
	IssuingAuthority string
	ArticleNo        string
	ArticleTitle     string
	SectionPath      []string
	Language         legal.Language
	EffectiveFrom    *time.Time

	// StructureConfidence carries through from ingestion, so a low-confidence
	// window chunk is identifiable at answer time and in evaluation.
	StructureConfidence legal.StructureConfidence

	Text string

	// Score is comparable only within one Retriever. After fusion it is the RRF
	// score; after reranking it is the reranker's.
	Score     float64
	Retriever Retriever
}

// Query is one retrieval request.
type Query struct {
	Text string

	// Language restricts results to one language. Empty searches both, which is
	// the default: an English question can be answered by an Amharic provision.
	Language legal.Language

	// AsOf is the date the law is asked about. Zero means today. Provisions not
	// in force on this date are excluded in SQL, never by the model.
	AsOf time.Time

	// PerRetriever is how many candidates each arm returns before fusion.
	PerRetriever int

	// Limit is how many fused candidates to return for reranking.
	Limit int
}

const (
	defaultPerRetriever = 50
	defaultLimit        = 50
)

func (q Query) withDefaults() Query {
	if q.PerRetriever <= 0 {
		q.PerRetriever = defaultPerRetriever
	}
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().UTC()
	}
	return q
}

// Service runs hybrid retrieval.
type Service struct {
	pool     *pgxpool.Pool
	embedder ai.Embedder
	table    string // embedding table for the configured model
}

// NewService wires retrieval to an embedding space.
//
// The embedder is resolved to a table here rather than per query, so a model
// with no table fails at construction instead of on a user's first question.
func NewService(pool *pgxpool.Pool, embedder ai.Embedder) (*Service, error) {
	table, err := legal.EmbeddingTable(embedder.Model(), embedder.Dimensions())
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, embedder: embedder, table: table}, nil
}

// Search retrieves candidates for a question.
//
// Three arms run and are fused with Reciprocal Rank Fusion:
//
//   - vector, which finds paraphrase and intent;
//   - keyword, which finds exact legal terminology;
//   - exact article lookup, which finds "Article 45" when that is literally
//     what was asked for.
//
// Vector-only misses citations, lexical-only misses intent, and neither
// reliably nails a direct article reference — which legal users type constantly.
func (s *Service) Search(ctx context.Context, q Query) ([]Result, error) {
	q = q.withDefaults()
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}

	var lists [][]Result

	// Exact article references first, and fused at rank 1, because a query that
	// names a provision is not ambiguous about what it wants.
	if refs := ParseArticleRefs(q.Text); len(refs) > 0 {
		exact, err := s.articleLookup(ctx, q, refs)
		if err != nil {
			return nil, fmt.Errorf("article lookup: %w", err)
		}
		if len(exact) > 0 {
			lists = append(lists, exact)
		}
	}

	vector, err := s.vectorSearch(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	if len(vector) > 0 {
		lists = append(lists, vector)
	}

	keyword, err := s.keywordSearch(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("keyword search: %w", err)
	}
	if len(keyword) > 0 {
		lists = append(lists, keyword)
	}

	fused := FuseRRF(lists...)
	if len(fused) > q.Limit {
		fused = fused[:q.Limit]
	}
	return fused, nil
}

// ---------------------------------------------------------------------------
// Shared SQL
//
// Every arm returns the same columns, so results are interchangeable after
// fusion. The live filter is repeated verbatim in each query rather than built
// by string concatenation: these are the predicates that keep repealed law out
// of answers, and they should be readable in place.
// ---------------------------------------------------------------------------

const selectColumns = `
	c.id, c.article_id, c.version_id, c.document_id,
	d.title, d.issuing_authority,
	a.article_no, a.title, a.section_path,
	c.language, v.effective_from, a.structure_confidence,
	c.text`

const fromChunks = `
	FROM chunks c
	JOIN articles a          ON a.id = c.article_id
	JOIN document_versions v ON v.id = c.version_id
	JOIN legal_documents d   ON d.id = c.document_id`

func scanResults(rows pgx.Rows, retriever Retriever) ([]Result, error) {
	defer rows.Close()

	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(
			&r.ChunkID, &r.ArticleID, &r.VersionID, &r.DocumentID,
			&r.DocumentTitle, &r.IssuingAuthority,
			&r.ArticleNo, &r.ArticleTitle, &r.SectionPath,
			&r.Language, &r.EffectiveFrom, &r.StructureConfidence,
			&r.Text, &r.Score,
		); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.Retriever = retriever
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Exact article lookup
// ---------------------------------------------------------------------------

// ArticleRef is a provision named directly in a query.
type ArticleRef struct {
	// Number is normalised to Arabic numerals, matching articles.article_no.
	Number string
	// Raw is what the user typed, kept for logging and debugging.
	Raw string
}

var (
	// "Article 45", "Art. 45", "Article 45(2)", "articles 45, 46 and 47".
	// The trailing group captures an enumeration, because legal questions
	// routinely name several provisions at once and dropping all but the first
	// would silently answer half the question.
	articleRefEnglish = regexp.MustCompile(
		`(?i)\b(?:articles?|arts?\.?)\s*(\d+[a-z]?(?:\s*(?:,|and|&)\s*\d+[a-z]?)*)`)

	// "አንቀጽ ፵፭", "አንቀጽ 45", "አንቀጽ ፵፭ እና ፵፮"
	articleRefAmharic = regexp.MustCompile(
		`አንቀጽ\s*((?:[\x{1369}-\x{137C}]+|\d+)(?:\s*(?:,|እና|&)\s*(?:[\x{1369}-\x{137C}]+|\d+))*)`)

	// Separators inside an enumeration.
	articleRefSeparator = regexp.MustCompile(`(?i)\s*(?:,|and|እና|&)\s*`)
)

// ParseArticleRefs finds article references in a query.
//
// This is the highest-leverage legal-specific behaviour in retrieval. Neither
// embeddings nor BM25 reliably return Article 45 for the query "Article 45":
// the number is a single low-information token to a lexical index, and to an
// embedding model "Article 45" and "Article 46" are nearly identical points.
// A direct lookup is exact, cheap, and exactly what the user asked for.
func ParseArticleRefs(query string) []ArticleRef {
	var (
		refs []ArticleRef
		seen = map[string]bool{}
	)

	// addList takes one match, which may be a single number or an enumeration.
	addList := func(matched string) {
		for _, raw := range articleRefSeparator.Split(matched, -1) {
			number := NormalizeNumber(raw)
			if number == "" || seen[number] {
				continue
			}
			seen[number] = true
			refs = append(refs, ArticleRef{Number: number, Raw: strings.TrimSpace(raw)})
		}
	}

	for _, m := range articleRefEnglish.FindAllStringSubmatch(query, -1) {
		addList(m[1])
	}
	for _, m := range articleRefAmharic.FindAllStringSubmatch(query, -1) {
		addList(m[1])
	}
	return refs
}

// ByArticles looks provisions up by article number.
//
// The same arm the hybrid search uses when a question names an article, exposed
// for the answer path's second retrieval pass: having read the first results,
// the model can name the article a cross-reference points at, and that is a
// lookup rather than a search.
func (s *Service) ByArticles(ctx context.Context, q Query, numbers []string) ([]Result, error) {
	refs := make([]ArticleRef, 0, len(numbers))
	seen := make(map[string]bool, len(numbers))
	for _, number := range numbers {
		number = strings.TrimSpace(number)
		if number == "" || seen[number] {
			continue
		}
		seen[number] = true
		refs = append(refs, ArticleRef{Number: number, Raw: number})
	}
	if len(refs) == 0 {
		return nil, nil
	}
	return s.articleLookup(ctx, q.withDefaults(), refs)
}

func (s *Service) articleLookup(ctx context.Context, q Query, refs []ArticleRef) ([]Result, error) {
	numbers := make([]string, len(refs))
	for i, r := range refs {
		numbers[i] = r.Number
	}

	// Score is constant: these are exact matches, and their value comes from
	// entering fusion at the top rank rather than from a similarity number.
	query := `
		SELECT` + selectColumns + `, 1.0::float8 AS score` + fromChunks + `
		WHERE a.article_no = ANY($1)
		  AND v.status = 'published'
		  AND (v.effective_from IS NULL OR v.effective_from <= $2)
		  AND (v.repealed_at IS NULL OR v.repealed_at > $2)
		  AND ($3 = '' OR c.language = $3)
		ORDER BY d.title, a.ordinal, c.ordinal
		LIMIT $4`

	rows, err := s.pool.Query(ctx, query, numbers, q.AsOf, string(q.Language), q.PerRetriever)
	if err != nil {
		return nil, err
	}
	return scanResults(rows, RetrieverArticle)
}

// ---------------------------------------------------------------------------
// Ge'ez numerals
//
// Mirrors workers/extraction/geez.py. The duplication is deliberate: the
// extractor normalises documents in Python at ingest time, and queries arrive
// in Go at request time. Both sides must agree, so both are tested against the
// same cases.
// ---------------------------------------------------------------------------

var geezValues = map[rune]int{
	'፩': 1, '፪': 2, '፫': 3, '፬': 4, '፭': 5, '፮': 6, '፯': 7, '፰': 8, '፱': 9,
	'፲': 10, '፳': 20, '፴': 30, '፵': 40, '፶': 50, '፷': 60, '፸': 70, '፹': 80, '፺': 90,
	'፻': 100, '፼': 10000,
}

// GeezToInt converts a Ge'ez numeral, returning ok=false if it is not one.
//
// Units and tens add within a group, ፻ multiplies the group before it by 100,
// and ፼ multiplies everything so far by 10,000 — so ፪፻፴፬ is 234.
func GeezToInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}

	total, group := 0, 0
	for _, r := range s {
		value, ok := geezValues[r]
		if !ok {
			return 0, false
		}
		switch value {
		case 100:
			if group == 0 {
				group = 1 // a bare ፻ is one hundred, not zero hundreds
			}
			total += group * 100
			group = 0
		case 10000:
			if total+group == 0 {
				total = 1
			} else {
				total += group
			}
			total *= 10000
			group = 0
		default:
			group += value
		}
	}
	return total + group, true
}

// NormalizeNumber renders an article number in Arabic numerals.
//
// Anything it does not recognise is returned trimmed and unchanged rather than
// guessed at: a number nobody understands should fail to match, not match the
// wrong provision.
func NormalizeNumber(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if n, ok := GeezToInt(raw); ok {
		return strconv.Itoa(n)
	}
	return strings.ToLower(raw)
}
