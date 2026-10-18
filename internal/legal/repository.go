package legal

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDuplicateDocument is returned when a document with the same title and
// language already exists. Ingesting the same text twice would double every
// retrieval result for it, so the database refuses and the caller decides
// whether this is a new version or a mistake.
var ErrDuplicateDocument = errors.New("document already exists")

// ErrDuplicateVersionLabel is returned when a document already has a version
// under this label. The caller decides whether that means "already done" or a
// genuine conflict — an amending proclamation and the code it amends can print
// the same number, and we never invent a suffixed label to get around it.
var ErrDuplicateVersionLabel = errors.New("version label already exists for this document")

// ErrAlreadyIngested is returned when these exact bytes are already in the
// corpus, under any document. Distinct from ErrDuplicateVersionLabel because
// the right response differs: this one is a no-op to report, that one is a
// conflict to look at.
var ErrAlreadyIngested = errors.New("this file has already been ingested")

// DBTX is the subset of pgx satisfied by both a pool and a transaction, so the
// same repository code runs inside or outside one. Ingestion always runs inside.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Repository is hand-written SQL over the legal corpus. Construct one per
// transaction.
type Repository struct {
	db DBTX
}

func NewRepository(db DBTX) *Repository { return &Repository{db: db} }

// EmbeddingRow is one vector ready to be written to an embedding table.
type EmbeddingRow struct {
	ChunkID   ChunkID
	Embedding []float32
}

// embeddingTables maps an embedding space to its table.
//
// Table names are interpolated into SQL, so they come from this allowlist and
// never from a caller. Adding a model means adding a migration and a line here.
var embeddingTables = map[string]string{
	"text-embedding-3-large|1536": "chunk_embeddings_openai_1536",
	"gemini-embedding-001|1536":   "chunk_embeddings_gemini_1536",
}

// EmbeddingTable resolves the table for a model and dimension count.
func EmbeddingTable(model string, dimensions int) (string, error) {
	key := fmt.Sprintf("%s|%d", model, dimensions)
	table, ok := embeddingTables[key]
	if !ok {
		return "", fmt.Errorf("no embedding table registered for model %q at %d dimensions: "+
			"add a migration and register it in legal.embeddingTables", model, dimensions)
	}
	return table, nil
}

// InsertDocument creates a document and returns its ID.
func (r *Repository) InsertDocument(ctx context.Context, d Document) (DocumentID, error) {
	const q = `
		INSERT INTO legal_documents (
			title, doc_type, legal_domain, issuing_authority,
			publication_date, effective_date, language, source_url, storage_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`

	var id DocumentID
	err := r.db.QueryRow(ctx, q,
		d.Title, d.DocType, d.LegalDomain, d.IssuingAuthority,
		d.PublicationDate, d.EffectiveDate, d.Language, d.SourceURL, d.StorageKey,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return "", fmt.Errorf("%w: %q (%s)", ErrDuplicateDocument, d.Title, d.Language)
		}
		return "", fmt.Errorf("insert document: %w", err)
	}
	return id, nil
}

// FindDocumentByTitle looks up a document by its unique (title, language) key.
// Returns pgx.ErrNoRows when absent.
func (r *Repository) FindDocumentByTitle(ctx context.Context, title string, lang Language) (Document, error) {
	const q = `
		SELECT id, title, doc_type, legal_domain, issuing_authority,
		       publication_date, effective_date, language, source_url, storage_key, created_at
		FROM legal_documents
		WHERE lower(title) = lower($1) AND language = $2`

	var d Document
	err := r.db.QueryRow(ctx, q, title, lang).Scan(
		&d.ID, &d.Title, &d.DocType, &d.LegalDomain, &d.IssuingAuthority,
		&d.PublicationDate, &d.EffectiveDate, &d.Language, &d.SourceURL, &d.StorageKey, &d.CreatedAt,
	)
	if err != nil {
		return Document{}, err
	}
	return d, nil
}

// InsertVersion creates a draft version and returns its ID.
func (r *Repository) InsertVersion(ctx context.Context, v Version) (VersionID, error) {
	const q = `
		INSERT INTO document_versions
			(document_id, version_label, effective_from, repealed_at, status,
			 source_checksum, source_url, storage_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`

	status := v.Status
	if status == "" {
		status = VersionDraft
	}

	// NULL, not "", for an unknown checksum: the unique index is partial on
	// IS NOT NULL, and empty strings would collide with each other.
	var checksum *string
	if v.SourceChecksum != "" {
		checksum = &v.SourceChecksum
	}

	var id VersionID
	err := r.db.QueryRow(ctx, q,
		v.DocumentID, v.Label, v.EffectiveFrom, v.RepealedAt, status,
		checksum, v.SourceURL, v.StorageKey).Scan(&id)
	if err != nil {
		// Two uniques on this table now, and reporting one as the other sends
		// an operator looking at the wrong thing. Switch on the constraint.
		switch constraintName(err) {
		case "document_versions_label_key":
			return "", fmt.Errorf("%w: %q", ErrDuplicateVersionLabel, v.Label)
		case "document_versions_source_checksum_key":
			return "", fmt.Errorf("%w: sha256 %s", ErrAlreadyIngested, v.SourceChecksum)
		}
		return "", fmt.Errorf("insert version: %w", err)
	}
	return id, nil
}

// FindVersionBySourceChecksum finds the version produced by a given file.
//
// This is the lookup that makes re-ingestion cheap. Ingestion embeds before it
// opens its transaction, so asking this question first is the difference
// between a re-run costing one SELECT and costing a document's worth of
// embedding calls followed by a rollback.
func (r *Repository) FindVersionBySourceChecksum(ctx context.Context, checksum string) (Version, error) {
	const q = `
		SELECT id, document_id, version_label, effective_from, repealed_at,
		       status, published_at, coalesce(source_checksum, ''), source_url, storage_key
		FROM document_versions
		WHERE source_checksum = $1`

	var v Version
	err := r.db.QueryRow(ctx, q, checksum).Scan(
		&v.ID, &v.DocumentID, &v.Label, &v.EffectiveFrom, &v.RepealedAt,
		&v.Status, &v.PublishedAt, &v.SourceChecksum, &v.SourceURL, &v.StorageKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Version{}, err
		}
		return Version{}, fmt.Errorf("find version by checksum: %w", err)
	}
	return v, nil
}

// FindVersionByLabel reports whether a document already carries this label.
func (r *Repository) FindVersionByLabel(ctx context.Context, documentID DocumentID, label string) (Version, error) {
	const q = `
		SELECT id, document_id, version_label, effective_from, repealed_at,
		       status, published_at, coalesce(source_checksum, ''), source_url, storage_key
		FROM document_versions
		WHERE document_id = $1 AND version_label = $2`

	var v Version
	err := r.db.QueryRow(ctx, q, documentID, label).Scan(
		&v.ID, &v.DocumentID, &v.Label, &v.EffectiveFrom, &v.RepealedAt,
		&v.Status, &v.PublishedAt, &v.SourceChecksum, &v.SourceURL, &v.StorageKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Version{}, err
		}
		return Version{}, fmt.Errorf("find version by label: %w", err)
	}
	return v, nil
}

// HasPublishedVersion reports whether a document already has a live version.
//
// The publish decision depends on it, so it is asked inside the publishing
// transaction: PublishVersion supersedes whatever is currently published before
// publishing, so two concurrent ingests that both checked beforehand would not
// collide — the second would silently demote the first.
func (r *Repository) HasPublishedVersion(ctx context.Context, documentID DocumentID) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM document_versions WHERE document_id = $1 AND status = 'published')`

	var exists bool
	if err := r.db.QueryRow(ctx, q, documentID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check published version: %w", err)
	}
	return exists, nil
}

// DeleteVersion removes a version and everything hanging off it.
//
// The only sanctioned way to re-ingest a document whose parse has improved.
// Articles, chunks and embeddings go with it via ON DELETE CASCADE. This is the
// one place the corpus is destructive, so it is never called implicitly — the
// operator asks for it by name.
func (r *Repository) DeleteVersion(ctx context.Context, versionID VersionID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM document_versions WHERE id = $1`, versionID)
	if err != nil {
		return fmt.Errorf("delete version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete version %s: no such version", versionID)
	}
	return nil
}

// InsertArticles writes articles and fills in their generated IDs.
//
// Pipelined as one batch: a 400-article code would otherwise cost 400 sequential
// round trips.
func (r *Repository) InsertArticles(ctx context.Context, articles []Article) error {
	if len(articles) == 0 {
		return nil
	}

	const q = `
		INSERT INTO articles (
			version_id, document_id, article_no, article_no_raw, title, chapter,
			section_path, text, ordinal, page_no, char_start, char_end, structure_confidence
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`

	batch := &pgx.Batch{}
	for _, a := range articles {
		sectionPath := a.SectionPath
		if sectionPath == nil {
			sectionPath = []string{}
		}
		batch.Queue(q,
			a.VersionID, a.DocumentID, a.Number, a.NumberRaw, a.Title, a.Chapter,
			sectionPath, a.Text, a.Ordinal, a.PageNo, a.CharStart, a.CharEnd, a.StructureConfidence)
	}

	results := r.db.SendBatch(ctx, batch)
	defer results.Close()

	for i := range articles {
		if err := results.QueryRow().Scan(&articles[i].ID); err != nil {
			return fmt.Errorf("insert article %s (ordinal %d): %w",
				articles[i].Number, articles[i].Ordinal, err)
		}
	}
	return results.Close()
}

// InsertChunks writes chunks and fills in their generated IDs.
func (r *Repository) InsertChunks(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	const q = `
		INSERT INTO chunks (
			article_id, version_id, document_id, language, text, embed_text, token_count, ordinal
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`

	batch := &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(q, c.ArticleID, c.VersionID, c.DocumentID, c.Language,
			c.Text, c.EmbedText, c.TokenCount, c.Ordinal)
	}

	results := r.db.SendBatch(ctx, batch)
	defer results.Close()

	for i := range chunks {
		if err := results.QueryRow().Scan(&chunks[i].ID); err != nil {
			return fmt.Errorf("insert chunk %d of article %s: %w",
				chunks[i].Ordinal, chunks[i].ArticleID, err)
		}
	}
	return results.Close()
}

// InsertEmbeddings writes vectors into the table for the given embedding space.
func (r *Repository) InsertEmbeddings(ctx context.Context, model, modelVersion string, dimensions int, rows []EmbeddingRow) error {
	if len(rows) == 0 {
		return nil
	}

	table, err := EmbeddingTable(model, dimensions)
	if err != nil {
		return err
	}

	// #nosec G201 -- table comes from the allowlist in EmbeddingTable, never
	// from a caller.
	q := fmt.Sprintf(`
		INSERT INTO %s (chunk_id, embedding, model, model_version)
		VALUES ($1, $2, $3, $4)`, table)

	batch := &pgx.Batch{}
	for _, row := range rows {
		if len(row.Embedding) != dimensions {
			return fmt.Errorf("chunk %s: embedding has %d dimensions, table %s expects %d",
				row.ChunkID, len(row.Embedding), table, dimensions)
		}
		batch.Queue(q, row.ChunkID, VectorLiteral(row.Embedding), model, modelVersion)
	}

	results := r.db.SendBatch(ctx, batch)
	defer results.Close()

	for range rows {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("insert embedding: %w", err)
		}
	}
	return results.Close()
}

// PublishVersion makes a version live, superseding whichever version was live
// before.
//
// Both statements are the caller's transaction, and the database's partial
// unique index refuses to hold two published versions of one document — so a
// half-applied publish cannot exist even if this code is wrong.
func (r *Repository) PublishVersion(ctx context.Context, documentID DocumentID, versionID VersionID) error {
	const supersede = `
		UPDATE document_versions
		SET status = 'superseded'
		WHERE document_id = $1 AND status = 'published' AND id <> $2`

	if _, err := r.db.Exec(ctx, supersede, documentID, versionID); err != nil {
		return fmt.Errorf("supersede previous version: %w", err)
	}

	const publish = `
		UPDATE document_versions
		SET status = 'published', published_at = $2
		WHERE id = $1`

	tag, err := r.db.Exec(ctx, publish, versionID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("publish version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("publish version %s: no such version", versionID)
	}
	return nil
}

// VectorLiteral renders a float32 slice in pgvector's text input format.
//
// pgx does not know the vector type, and registering a custom codec for one
// INSERT path is more machinery than the format deserves: it is a
// bracket-delimited list, and Postgres parses it on the way in.
func VectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	// Pre-size for the common case of "-0.123456789," per element.
	buf := make([]byte, 0, len(v)*14+2)
	buf = append(buf, '[')
	for i, f := range v {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendFloat(buf, float64(f), 'f', -1, 32)
	}
	return string(append(buf, ']'))
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// constraintName returns the constraint a unique violation names, or "" if the
// error is not one. Which constraint fired is the whole message: a table with
// two uniques cannot report either accurately without it.
func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}
