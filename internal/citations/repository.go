package citations

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// ErrNotFound is returned when a chunk ID does not resolve.
//
// This is not a display glitch. An unresolvable citation in a delivered answer
// means the answer pointed at something the database does not have, which is the
// definition of ungrounded.
var ErrNotFound = errors.New("citation not found")

// Repository reads citations from Postgres.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const citationColumns = `
	c.id, c.article_id, c.version_id, c.document_id,
	d.title, d.issuing_authority, d.source_url,
	a.article_no, a.title, a.section_path, a.page_no, a.char_start, a.char_end,
	c.language, v.version_label, v.effective_from, v.repealed_at,
	c.text`

// Resolve returns citations for the given chunk IDs, in the order requested.
//
// A missing ID is an error rather than a silent omission: the caller is about to
// show these to a user as the sources of a legal answer.
func (r *Repository) Resolve(ctx context.Context, ids []legal.ChunkID) ([]Citation, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = string(id)
	}

	query := `
		SELECT` + citationColumns + `
		FROM chunks c
		JOIN articles a          ON a.id = c.article_id
		JOIN document_versions v ON v.id = c.version_id
		JOIN legal_documents d   ON d.id = c.document_id
		WHERE c.id = ANY($1)`

	rows, err := r.pool.Query(ctx, query, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve citations: %w", err)
	}
	defer rows.Close()

	found := make(map[legal.ChunkID]Citation, len(ids))
	for rows.Next() {
		c, err := scanCitation(rows)
		if err != nil {
			return nil, err
		}
		found[c.ChunkID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve citations: %w", err)
	}

	out := make([]Citation, 0, len(ids))
	for _, id := range ids {
		c, ok := found[id]
		if !ok {
			return nil, fmt.Errorf("%w: chunk %s", ErrNotFound, id)
		}
		out = append(out, c)
	}
	return out, nil
}

// ResolveSome returns the citations that exist, in the order requested, and
// silently skips the ones that do not.
//
// Deliberately lenient, unlike Resolve. This serves a different purpose: an
// answer carrying forward the sources a conversation already cited. A chunk id
// from a browser tab open across a re-ingestion should cost that one source, not
// the whole answer. Resolve stays strict, because there an unresolvable id is a
// grounding failure in something already delivered.
func (r *Repository) ResolveSome(ctx context.Context, ids []legal.ChunkID) ([]Citation, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = string(id)
	}

	query := `
		SELECT` + citationColumns + `
		FROM chunks c
		JOIN articles a          ON a.id = c.article_id
		JOIN document_versions v ON v.id = c.version_id
		JOIN legal_documents d   ON d.id = c.document_id
		WHERE c.id = ANY($1)`

	rows, err := r.pool.Query(ctx, query, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve citations: %w", err)
	}
	defer rows.Close()

	found := make(map[legal.ChunkID]Citation, len(ids))
	for rows.Next() {
		c, err := scanCitation(rows)
		if err != nil {
			return nil, err
		}
		found[c.ChunkID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve citations: %w", err)
	}

	out := make([]Citation, 0, len(found))
	for _, id := range ids {
		if c, ok := found[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// Get resolves a single citation.
func (r *Repository) Get(ctx context.Context, id legal.ChunkID) (Citation, error) {
	query := `
		SELECT` + citationColumns + `
		FROM chunks c
		JOIN articles a          ON a.id = c.article_id
		JOIN document_versions v ON v.id = c.version_id
		JOIN legal_documents d   ON d.id = c.document_id
		WHERE c.id = $1`

	rows, err := r.pool.Query(ctx, query, string(id))
	if err != nil {
		return Citation{}, fmt.Errorf("get citation: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Citation{}, fmt.Errorf("get citation: %w", err)
		}
		return Citation{}, fmt.Errorf("%w: chunk %s", ErrNotFound, id)
	}
	return scanCitation(rows)
}

func scanCitation(rows pgx.Rows) (Citation, error) {
	var c Citation
	if err := rows.Scan(
		&c.ChunkID, &c.ArticleID, &c.VersionID, &c.DocumentID,
		&c.DocumentTitle, &c.IssuingAuthority, &c.SourceURL,
		&c.ArticleNo, &c.ArticleTitle, &c.SectionPath,
		&c.PageNo, &c.CharStart, &c.CharEnd,
		&c.Language, &c.VersionLabel, &c.EffectiveFrom, &c.RepealedAt,
		&c.Text,
	); err != nil {
		return Citation{}, fmt.Errorf("scan citation: %w", err)
	}
	return c, nil
}
