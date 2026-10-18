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

	// PublishPolicy decides whether the version goes live at the end of the
	// same transaction. A document ingested without publishing is invisible to
	// retrieval, which is the right default for a corpus nobody has read yet.
	PublishPolicy PublishPolicy

	// SourceChecksum is the sha256 of the file this extraction came from.
	// Optional, but supplying it is what makes a re-ingest cheap: it is checked
	// before any embedding is paid for, and it is unique across the corpus.
	SourceChecksum string

	// Replace deletes the version these bytes previously produced, so a
	// document can be re-ingested after the extractor improves. Destructive and
	// never implicit — without it, an already-ingested file is a no-op.
	Replace bool
}

// Result summarises what was written.
type Result struct {
	DocumentID    legal.DocumentID `json:"document_id"`
	VersionID     legal.VersionID  `json:"version_id"`
	Articles      int              `json:"articles"`
	Chunks        int              `json:"chunks"`
	LowConfidence int              `json:"low_confidence_articles"`
	Published     bool             `json:"published"`

	// AlreadyIngested reports that these bytes were already in the corpus and
	// nothing was written. Not an error: it is the expected outcome of a second
	// run, and the whole point of checking before embedding.
	AlreadyIngested bool `json:"already_ingested"`

	// Replaced reports that an earlier version produced by the same file was
	// deleted to make room for this one.
	Replaced         bool   `json:"replaced,omitempty"`
	EmbeddingModel   string `json:"embedding_model"`
	EmbeddingVersion string `json:"embedding_version"`
	DurationMS       int64  `json:"duration_ms"`
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

	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	if err := req.Extraction.Validate(); err != nil {
		return Result{}, err
	}
	if req.Document.Language != legal.Language(req.Extraction.Language) {
		return Result{}, fmt.Errorf(
			"document language %q does not match extraction language %q",
			req.Document.Language, req.Extraction.Language)
	}

	// Pre-flight. Every check that can reject this request runs here, before
	// EmbedAll, because EmbedAll is minutes of paid network I/O and a rejection
	// after it has cost real money for nothing. Anything that can only be known
	// inside the transaction — whether a sibling version is live — stays there.
	supersede, err := s.preflight(ctx, req)
	switch {
	case errors.Is(err, legal.ErrAlreadyIngested):
		s.log.InfoContext(ctx, "already ingested, nothing to do",
			slog.String("title", req.Document.Title),
			slog.String("sha256", req.SourceChecksum))
		return Result{AlreadyIngested: true, DurationMS: time.Since(start).Milliseconds()}, nil
	case err != nil:
		return Result{}, err
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

	// The replaced version goes first, inside this transaction, so that a
	// failure anywhere below leaves the old one exactly where it was. It also
	// frees that version's label for the row about to be inserted.
	if supersede != "" {
		s.log.InfoContext(ctx, "replacing existing version",
			slog.String("version_id", string(supersede)),
			slog.String("sha256", req.SourceChecksum))
		if err := repo.DeleteVersion(ctx, supersede); err != nil {
			return Result{}, err
		}
	}

	documentID, err := s.resolveDocument(ctx, repo, req.Document)
	if err != nil {
		return Result{}, err
	}

	version := req.Version
	version.DocumentID = documentID
	version.Status = legal.VersionDraft
	version.SourceChecksum = req.SourceChecksum
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

	// Resolved here, not by the caller: PublishVersion supersedes whatever is
	// currently published before it publishes, so a check made outside this
	// transaction is a race that silently demotes a live version rather than
	// failing loudly.
	publish := req.PublishPolicy == PublishAlways
	if req.PublishPolicy == PublishIfNoneLive {
		live, err := repo.HasPublishedVersion(ctx, documentID)
		if err != nil {
			return Result{}, err
		}
		publish = !live
		if live {
			s.log.InfoContext(ctx, "left as draft: document already has a published version",
				slog.String("title", req.Document.Title),
				slog.String("document_id", string(documentID)))
		}
	}
	if publish {
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
		Published:        publish,
		Replaced:         supersede != "",
		EmbeddingModel:   s.embedder.Model(),
		EmbeddingVersion: s.embedder.Version(),
		DurationMS:       time.Since(start).Milliseconds(),
	}

	if result.Replaced && !result.Published {
		// The version that was replaced may well have been the published one,
		// in which case this document just left retrieval entirely. That is a
		// defensible outcome — the new parse did not earn publication — but it
		// is not one to discover later.
		s.log.WarnContext(ctx, "replaced version is not published; this document "+
			"is no longer visible to retrieval until someone publishes a version",
			slog.String("title", req.Document.Title),
			slog.String("document_id", string(documentID)))
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
		slog.Bool("replaced", result.Replaced),
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

// preflight runs every rejection that does not need the write transaction,
// before a single embedding is paid for.
//
// The ordering in Ingest is deliberate: embedding a legal code is minutes of
// network I/O against a metered provider, and before this existed a re-run of
// an already-ingested document paid all of it and *then* failed on a unique
// violation — on a free tier, a meaningful slice of a day's quota spent to
// learn something a SELECT could have said immediately.
//
// It reports whether an existing version was deleted to make room, and returns
// legal.ErrAlreadyIngested when there is nothing to do.
func (s *Service) preflight(ctx context.Context, req Request) (supersede legal.VersionID, err error) {
	repo := legal.NewRepository(s.pool)

	// 1. These exact bytes. Checked first because it is the cheapest certain
	//    answer: same sha256 means same document, whatever the title says.
	if req.SourceChecksum != "" {
		existing, err := repo.FindVersionBySourceChecksum(ctx, req.SourceChecksum)
		switch {
		case err == nil && !req.Replace:
			return "", fmt.Errorf("%w (version %s)", legal.ErrAlreadyIngested, existing.ID)
		case err == nil:
			// Replace: note which version has to go, but do NOT delete it here.
			//
			// Deleting now would put the destruction before EmbedAll, which is
			// ten minutes of metered network I/O that routinely fails — a
			// quota ceiling, a dropped connection, an impatient Ctrl-C. The old
			// version would already be gone, with nothing to replace it, and a
			// law would simply vanish from the corpus. The delete therefore
			// happens inside the write transaction, where a failure rolls it
			// back along with everything else.
			supersede = existing.ID
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return "", err
		}
	}

	// 2. The label, which only means anything once we know the document. A
	//    miss here is the normal case for a new document and costs one query.
	doc, err := repo.FindDocumentByTitle(ctx, req.Document.Title, req.Document.Language)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return supersede, nil
	case err != nil:
		return "", fmt.Errorf("look up document: %w", err)
	}

	clash, err := repo.FindVersionByLabel(ctx, doc.ID, req.Version.Label)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return "", err
	case clash.ID == supersede:
		// The label is held by the very version this run is replacing, which
		// is not a conflict — it is the point.
	default:
		// A real conflict: the bytes differ (or no checksum was given) but the
		// label is taken. We refuse rather than invent a suffixed label — a
		// fabricated version label is a citation that looks authoritative and
		// refers to nothing.
		return "", fmt.Errorf("%w: document %q already has version %q",
			legal.ErrDuplicateVersionLabel, doc.Title, req.Version.Label)
	}

	return supersede, nil
}
