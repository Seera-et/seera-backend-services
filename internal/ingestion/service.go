package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Request is one document to ingest: its metadata, plus the extraction produced
// by workers/extraction.
type Request struct {
	Document   legal.Document
	Version    legal.Version
	Extraction Extraction

	// Publish makes the version live at the end of the same transaction.
	// A document ingested without publishing is invisible to retrieval, which
	// is the right default for a corpus that has not been eyeballed yet.
	Publish bool
}

// Result summarises what was written.
type Result struct {
	DocumentID       legal.DocumentID `json:"document_id"`
	VersionID        legal.VersionID  `json:"version_id"`
	Articles         int              `json:"articles"`
	Chunks           int              `json:"chunks"`
	LowConfidence    int              `json:"low_confidence_articles"`
	Published        bool             `json:"published"`
	EmbeddingModel   string           `json:"embedding_model"`
	EmbeddingVersion string           `json:"embedding_version"`
	DurationMS       int64            `json:"duration_ms"`
}

// Embedder is the slice of embedding this service needs.
type Embedder interface {
	EmbedAll(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Version() string
	Dimensions() int
}

// Service ingests extracted documents into the corpus.
type Service struct {
	pool     *pgxpool.Pool
	embedder Embedder
	log      *slog.Logger
}

func NewService(pool *pgxpool.Pool, embedder Embedder, log *slog.Logger) *Service {
	return &Service{pool: pool, embedder: embedder, log: log}
}

// Ingest validates, chunks, embeds and stores a document.
//
// Ordering matters here. Embedding happens *before* the transaction opens:
// embedding a 400-article code is minutes of network I/O, and holding a
// Postgres transaction open across it would pin a connection and leave a long
// window in which a crash rolls everything back anyway. Once the vectors are in
// hand, the write is a single short transaction — version, articles, chunks,
// embeddings, publish — so a failure at any point leaves nothing behind. There
// is no state in which a document is half-indexed and answering questions.
func (s *Service) Ingest(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	if err := req.Extraction.Validate(); err != nil {
		return Result{}, err
	}
	if req.Document.Language != legal.Language(req.Extraction.Language) {
		return Result{}, fmt.Errorf(
			"document language %q does not match extraction language %q",
			req.Document.Language, req.Extraction.Language)
	}

	// Domain objects first, with placeholder IDs; the database assigns the real
	// ones on insert.
	articles := req.Extraction.toArticles("", "")

	// Chunk before embedding so the whole corpus of texts is known up front and
	// can be embedded in batches rather than per article.
	type chunkPlan struct {
		articleIdx int
		chunk      legal.Chunk
	}
	var (
		plans []chunkPlan
		texts []string
	)
	for i, a := range articles {
		chunks, err := Chunk(req.Document, a)
		if err != nil {
			return Result{}, fmt.Errorf("chunk article %s: %w", a.Number, err)
		}
		for _, c := range chunks {
			plans = append(plans, chunkPlan{articleIdx: i, chunk: c})
			texts = append(texts, c.EmbedText)
		}
	}

	s.log.InfoContext(ctx, "embedding document",
		slog.String("title", req.Document.Title),
		slog.Int("articles", len(articles)),
		slog.Int("chunks", len(texts)),
		slog.String("model", s.embedder.Model()))

	vectors, err := s.embedder.EmbedAll(ctx, texts)
	if err != nil {
		return Result{}, fmt.Errorf("embed document: %w", err)
	}
	if len(vectors) != len(plans) {
		return Result{}, fmt.Errorf("got %d vectors for %d chunks", len(vectors), len(plans))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	repo := legal.NewRepository(tx)

	documentID, err := s.resolveDocument(ctx, repo, req.Document)
	if err != nil {
		return Result{}, err
	}

	version := req.Version
	version.DocumentID = documentID
	version.Status = legal.VersionDraft
	versionID, err := repo.InsertVersion(ctx, version)
	if err != nil {
		return Result{}, err
	}

	for i := range articles {
		articles[i].DocumentID = documentID
		articles[i].VersionID = versionID
	}
	if err := repo.InsertArticles(ctx, articles); err != nil {
		return Result{}, err
	}

	chunks := make([]legal.Chunk, 0, len(plans))
	for _, p := range plans {
		c := p.chunk
		c.ArticleID = articles[p.articleIdx].ID
		c.DocumentID = documentID
		c.VersionID = versionID
		chunks = append(chunks, c)
	}
	if err := repo.InsertChunks(ctx, chunks); err != nil {
		return Result{}, err
	}

	rows := make([]legal.EmbeddingRow, len(chunks))
	for i := range chunks {
		rows[i] = legal.EmbeddingRow{ChunkID: chunks[i].ID, Embedding: vectors[i]}
	}
	if err := repo.InsertEmbeddings(ctx,
		s.embedder.Model(), s.embedder.Version(), s.embedder.Dimensions(), rows); err != nil {
		return Result{}, err
	}

	if req.Publish {
		if err := repo.PublishVersion(ctx, documentID, versionID); err != nil {
			return Result{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}

	result := Result{
		DocumentID:       documentID,
		VersionID:        versionID,
		Articles:         len(articles),
		Chunks:           len(chunks),
		LowConfidence:    req.Extraction.LowConfidenceCount(),
		Published:        req.Publish,
		EmbeddingModel:   s.embedder.Model(),
		EmbeddingVersion: s.embedder.Version(),
		DurationMS:       time.Since(start).Milliseconds(),
	}

	level := slog.LevelInfo
	if result.LowConfidence > 0 {
		// Worth surfacing loudly: these articles were not recovered from the
		// document's real structure, and their chunks will retrieve worse.
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "document ingested",
		slog.String("title", req.Document.Title),
		slog.String("document_id", string(documentID)),
		slog.String("version_id", string(versionID)),
		slog.Int("articles", result.Articles),
		slog.Int("chunks", result.Chunks),
		slog.Int("low_confidence_articles", result.LowConfidence),
		slog.Bool("published", result.Published),
		slog.Int64("duration_ms", result.DurationMS))

	return result, nil
}

// resolveDocument reuses an existing document row when one matches, so a second
// version of the Commercial Code attaches to the same document rather than
// creating a duplicate.
func (s *Service) resolveDocument(ctx context.Context, repo *legal.Repository, d legal.Document) (legal.DocumentID, error) {
	existing, err := repo.FindDocumentByTitle(ctx, d.Title, d.Language)
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "attaching version to existing document",
			slog.String("title", d.Title),
			slog.String("document_id", string(existing.ID)))
		return existing.ID, nil
	case errors.Is(err, pgx.ErrNoRows):
		return repo.InsertDocument(ctx, d)
	default:
		return "", fmt.Errorf("look up document: %w", err)
	}
}
