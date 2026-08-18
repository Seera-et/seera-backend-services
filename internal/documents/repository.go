package documents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// ErrNotFound is returned when a document or version is not visible.
//
// Not visible covers both "no such row" and "exists but is only a draft". The
// two are one answer on purpose: whether an unpublished document exists is not
// something the browse API should reveal.
var ErrNotFound = errors.New("not found")

// Repository reads the catalogue from Postgres.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// visibleVersions is the one place that decides what a reader may see.
//
// Published is the live text; superseded is history a reader is entitled to
// read, and the version metadata says which is which. Draft is never visible —
// the same rule retrieval applies, expressed once.
const visibleVersions = `v.status IN ('published', 'superseded')`

// List returns a page of the catalogue, plus the total matching the filter.
//
// One query: the window count gives the total before LIMIT is applied, which
// avoids a second round trip that could disagree with the first.
//
// The filters are written as "$n = empty OR predicate" rather than assembled
// into SQL text. That keeps the statement parameterised and single-plan; the
// catalogue is small enough that not hitting an index on title is irrelevant,
// and correctness here is worth more than a plan.
func (r *Repository) List(ctx context.Context, f ListFilter) (ListResult, error) {
	const query = `
		SELECT
			d.id, d.title, d.doc_type, d.legal_domain, d.issuing_authority,
			d.language, d.publication_date, d.effective_date, d.source_url,
			v.id, v.version_label, v.status, v.effective_from, v.repealed_at, v.published_at,
			(SELECT count(*) FROM articles a WHERE a.version_id = v.id) AS article_count,
			count(*) OVER () AS total
		FROM legal_documents d
		JOIN document_versions v
			ON v.document_id = d.id AND v.status = 'published'
		WHERE ($1 = '' OR d.title ILIKE '%' || $1 || '%'
		                OR d.issuing_authority ILIKE '%' || $1 || '%')
		  AND ($2 = '' OR d.language = $2)
		  AND ($3 = '' OR d.doc_type = $3)
		ORDER BY d.title, d.id
		LIMIT $4 OFFSET $5`

	rows, err := r.pool.Query(ctx, query,
		f.Term, string(f.Language), string(f.DocType), f.Limit, f.Offset)
	if err != nil {
		return ListResult{}, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	result := ListResult{
		Documents: []DocumentSummary{},
		Limit:     f.Limit,
		Offset:    f.Offset,
	}

	for rows.Next() {
		var (
			d     DocumentSummary
			total int
		)
		if err := rows.Scan(
			&d.ID, &d.Title, &d.DocType, &d.LegalDomain, &d.IssuingAuthority,
			&d.Language, &d.PublicationDate, &d.EffectiveDate, &d.SourceURL,
			&d.Version.ID, &d.Version.Label, &d.Version.Status,
			&d.Version.EffectiveFrom, &d.Version.RepealedAt, &d.Version.PublishedAt,
			&d.Version.ArticleCount, &total,
		); err != nil {
			return ListResult{}, fmt.Errorf("scan document: %w", err)
		}
		result.Documents = append(result.Documents, d)
		result.Total = total
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, fmt.Errorf("list documents: %w", err)
	}

	return result, nil
}

// Get returns one document with the versions a reader may see.
func (r *Repository) Get(ctx context.Context, id legal.DocumentID) (Detail, error) {
	const documentQuery = `
		SELECT
			d.id, d.title, d.doc_type, d.legal_domain, d.issuing_authority,
			d.language, d.publication_date, d.effective_date, d.source_url,
			v.id, v.version_label, v.status, v.effective_from, v.repealed_at, v.published_at,
			(SELECT count(*) FROM articles a WHERE a.version_id = v.id)
		FROM legal_documents d
		JOIN document_versions v
			ON v.document_id = d.id AND v.status = 'published'
		WHERE d.id = $1`

	var detail Detail
	row := r.pool.QueryRow(ctx, documentQuery, string(id))
	d := &detail.Document
	if err := row.Scan(
		&d.ID, &d.Title, &d.DocType, &d.LegalDomain, &d.IssuingAuthority,
		&d.Language, &d.PublicationDate, &d.EffectiveDate, &d.SourceURL,
		&d.Version.ID, &d.Version.Label, &d.Version.Status,
		&d.Version.EffectiveFrom, &d.Version.RepealedAt, &d.Version.PublishedAt,
		&d.Version.ArticleCount,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Detail{}, fmt.Errorf("%w: document %s", ErrNotFound, id)
		}
		return Detail{}, fmt.Errorf("get document: %w", err)
	}

	versions, err := r.versions(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	detail.Versions = versions

	return detail, nil
}

func (r *Repository) versions(ctx context.Context, id legal.DocumentID) ([]VersionSummary, error) {
	query := `
		SELECT
			v.id, v.version_label, v.status, v.effective_from, v.repealed_at, v.published_at,
			(SELECT count(*) FROM articles a WHERE a.version_id = v.id)
		FROM document_versions v
		WHERE v.document_id = $1 AND ` + visibleVersions + `
		ORDER BY (v.status = 'published') DESC, v.effective_from DESC NULLS LAST, v.version_label DESC`

	rows, err := r.pool.Query(ctx, query, string(id))
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	versions := []VersionSummary{}
	for rows.Next() {
		var v VersionSummary
		if err := rows.Scan(
			&v.ID, &v.Label, &v.Status, &v.EffectiveFrom, &v.RepealedAt,
			&v.PublishedAt, &v.ArticleCount,
		); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	return versions, nil
}

// ResolveVersion picks the version a request refers to.
//
// An empty versionID means the published one. A version that belongs to another
// document, or is still a draft, is reported as not found rather than served —
// the version id in a URL is user input like any other.
func (r *Repository) ResolveVersion(
	ctx context.Context,
	documentID legal.DocumentID,
	versionID legal.VersionID,
) (VersionSummary, error) {
	query := `
		SELECT
			v.id, v.version_label, v.status, v.effective_from, v.repealed_at, v.published_at,
			(SELECT count(*) FROM articles a WHERE a.version_id = v.id)
		FROM document_versions v
		WHERE v.document_id = $1
		  AND ` + visibleVersions + `
		  AND ($2 = '' OR v.id = $2)
		ORDER BY (v.status = 'published') DESC, v.effective_from DESC NULLS LAST
		LIMIT 1`

	var v VersionSummary
	err := r.pool.QueryRow(ctx, query, string(documentID), string(versionID)).Scan(
		&v.ID, &v.Label, &v.Status, &v.EffectiveFrom, &v.RepealedAt,
		&v.PublishedAt, &v.ArticleCount,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VersionSummary{}, fmt.Errorf("%w: version for document %s", ErrNotFound, documentID)
		}
		return VersionSummary{}, fmt.Errorf("resolve version: %w", err)
	}
	return v, nil
}

// Articles returns one page of a version's articles, in document order.
func (r *Repository) Articles(
	ctx context.Context,
	versionID legal.VersionID,
	after, limit int,
) ([]Article, error) {
	const query = `
		SELECT
			a.id, a.article_no, a.article_no_raw, a.title, a.chapter, a.section_path,
			a.text, a.ordinal, a.page_no, a.char_start, a.char_end, a.structure_confidence,
			(SELECT c.id FROM chunks c WHERE c.article_id = a.id ORDER BY c.ordinal LIMIT 1)
		FROM articles a
		WHERE a.version_id = $1 AND a.ordinal > $2
		ORDER BY a.ordinal
		LIMIT $3`

	rows, err := r.pool.Query(ctx, query, string(versionID), after, limit)
	if err != nil {
		return nil, fmt.Errorf("list articles: %w", err)
	}
	defer rows.Close()

	articles := []Article{}
	for rows.Next() {
		var (
			a       Article
			chunkID *string
		)
		if err := rows.Scan(
			&a.ID, &a.ArticleNo, &a.ArticleNoRaw, &a.Title, &a.Chapter, &a.SectionPath,
			&a.Text, &a.Ordinal, &a.PageNo, &a.CharStart, &a.CharEnd, &a.StructureConfidence,
			&chunkID,
		); err != nil {
			return nil, fmt.Errorf("scan article: %w", err)
		}
		// An article with no chunk was never indexed, so it is not citable. The
		// field stays empty rather than carrying a zero uuid that resolves to
		// nothing.
		if chunkID != nil {
			a.ChunkID = legal.ChunkID(*chunkID)
		}
		articles = append(articles, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list articles: %w", err)
	}
	return articles, nil
}

// Stats counts the corpus over published versions only.
//
// Counted, never estimated: these numbers appear on the home page as a claim
// about what the system can answer from, so they have to be the same set
// retrieval sees.
func (r *Repository) Stats(ctx context.Context) (Stats, error) {
	const totalsQuery = `
		SELECT
			(SELECT count(DISTINCT v.document_id) FROM document_versions v WHERE v.status = 'published'),
			(SELECT count(*) FROM document_versions v WHERE v.status = 'published'),
			(SELECT count(*) FROM articles a
			   JOIN document_versions v ON v.id = a.version_id AND v.status = 'published'),
			(SELECT count(*) FROM chunks c
			   JOIN document_versions v ON v.id = c.version_id AND v.status = 'published'),
			(SELECT max(v.published_at) FROM document_versions v WHERE v.status = 'published')`

	var s Stats
	if err := r.pool.QueryRow(ctx, totalsQuery).Scan(
		&s.Documents, &s.Versions, &s.Articles, &s.Chunks, &s.LastPublishedAt,
	); err != nil {
		return Stats{}, fmt.Errorf("corpus totals: %w", err)
	}

	const byLanguageQuery = `
		SELECT d.language,
		       count(DISTINCT d.id),
		       count(a.id)
		FROM legal_documents d
		JOIN document_versions v ON v.document_id = d.id AND v.status = 'published'
		LEFT JOIN articles a     ON a.version_id = v.id
		GROUP BY d.language
		ORDER BY d.language`

	rows, err := r.pool.Query(ctx, byLanguageQuery)
	if err != nil {
		return Stats{}, fmt.Errorf("corpus languages: %w", err)
	}
	defer rows.Close()

	s.Languages = []LanguageStat{}
	for rows.Next() {
		var stat LanguageStat
		if err := rows.Scan(&stat.Language, &stat.Documents, &stat.Articles); err != nil {
			return Stats{}, fmt.Errorf("scan language stat: %w", err)
		}
		s.Languages = append(s.Languages, stat)
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("corpus languages: %w", err)
	}

	const byTypeQuery = `
		SELECT d.doc_type, count(DISTINCT d.id)
		FROM legal_documents d
		JOIN document_versions v ON v.document_id = d.id AND v.status = 'published'
		GROUP BY d.doc_type
		ORDER BY count(DISTINCT d.id) DESC, d.doc_type`

	typeRows, err := r.pool.Query(ctx, byTypeQuery)
	if err != nil {
		return Stats{}, fmt.Errorf("corpus types: %w", err)
	}
	defer typeRows.Close()

	s.DocTypes = []TypeStat{}
	for typeRows.Next() {
		var stat TypeStat
		if err := typeRows.Scan(&stat.DocType, &stat.Documents); err != nil {
			return Stats{}, fmt.Errorf("scan type stat: %w", err)
		}
		s.DocTypes = append(s.DocTypes, stat)
	}
	if err := typeRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("corpus types: %w", err)
	}

	return s, nil
}
