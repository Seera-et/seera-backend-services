//go:build integration

// Package integration exercises ingestion against a real Postgres.
//
// The behaviour that matters here cannot be tested with a fake: it is the
// interaction between the pre-flight queries, the unique constraints, and the
// order in which embedding happens relative to the transaction. A mock of pgx
// would mostly test the mock.
//
//	docker compose up -d
//	DATABASE_URL=postgres://seera:seera@localhost:5432/seera?sslmode=disable \
//	    go test -tags integration ./tests/integration/... -v
//
// Skips when DATABASE_URL is unset. Every test works in its own document, so a
// run leaves the corpus it found otherwise intact.
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/database"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func connect(t *testing.T) *database.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; this test needs a real Postgres with pgvector")
	}

	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := database.Migrate(ctx, pool, testLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// countingEmbedder returns deterministic vectors and counts how many texts it
// was asked to embed. The count is the assertion in the test that matters most:
// embedding is the expensive, metered step, and the whole point of pre-flight
// is that a repeat ingest never reaches it.
type countingEmbedder struct {
	texts int
	// failAfter makes EmbedAll fail once it has been asked for this many texts
	// in total, standing in for the quota ceiling or dropped connection that a
	// ten-minute embedding run routinely meets.
	failAfter int
}

func (c *countingEmbedder) Model() string   { return "gemini-embedding-001" }
func (c *countingEmbedder) Version() string { return "d1536" }
func (c *countingEmbedder) Dimensions() int { return 1536 }

func (c *countingEmbedder) EmbedAll(_ context.Context, texts []string) ([][]float32, error) {
	c.texts += len(texts)
	if c.failAfter > 0 && c.texts >= c.failAfter {
		return nil, errors.New("quota exhausted")
	}
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, 1536)
		v[i%1536] = 1
		out[i] = v
	}
	return out, nil
}

func uniqueRequest(t *testing.T, checksum string) ingestion.Request {
	t.Helper()
	title := fmt.Sprintf("Integration Test Proclamation %d", time.Now().UnixNano())
	return ingestion.Request{
		Document: legal.Document{
			Title:    title,
			DocType:  legal.DocTypeProclamation,
			Language: legal.LanguageEnglish,
		},
		Version:        legal.Version{Label: "2021"},
		PublishPolicy:  ingestion.PublishIfNoneLive,
		SourceChecksum: checksum,
		Extraction: ingestion.Extraction{
			Language:  "en",
			PageCount: 2,
			Articles: []ingestion.ExtractedArticle{
				{ArticleNo: "1", Title: "Short title", Text: "This Proclamation may be cited as the Test Proclamation.", StructureConfidence: "high"},
				{ArticleNo: "2", Title: "Definitions", Text: "In this Proclamation the following terms shall have the meanings given to them.", StructureConfidence: "high"},
			},
		},
	}
}

func checksum(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%064x", time.Now().UnixNano())
}

// This is the test the whole pre-flight design exists for.
//
// Ingest embeds outside its transaction, so before pre-flight a re-run of an
// already-ingested document paid the full embedding cost — minutes, and a
// meaningful slice of a free-tier daily quota — and only then failed on a
// unique violation. The assertion is not "it returned an error"; it is that the
// embedder was never called.
func TestReingestingTheSameFileCostsNoEmbeddings(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	embedder := &countingEmbedder{}
	svc := ingestion.NewService(pool.Pool, embedder, testLogger())
	req := uniqueRequest(t, checksum(t))

	first, err := svc.Ingest(ctx, req)
	if err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}
	if first.Chunks == 0 {
		t.Fatal("first ingest wrote no chunks")
	}
	afterFirst := embedder.texts
	if afterFirst == 0 {
		t.Fatal("first ingest embedded nothing")
	}

	second, err := svc.Ingest(ctx, req)
	if err != nil {
		t.Fatalf("second Ingest() = %v", err)
	}
	if !second.AlreadyIngested {
		t.Error("second ingest did not report AlreadyIngested")
	}
	if embedder.texts != afterFirst {
		t.Errorf("second ingest embedded %d more texts; pre-flight should have stopped "+
			"before the embedder was touched", embedder.texts-afterFirst)
	}
}

// Every chunk must have a vector. A shortfall is not a degraded document — the
// chunks without one are unretrievable, permanently, and nothing downstream
// reports it.
func TestEveryChunkGetsAVector(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	svc := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger())
	result, err := svc.Ingest(ctx, uniqueRequest(t, checksum(t)))
	if err != nil {
		t.Fatalf("Ingest() = %v", err)
	}

	var chunks, vectors int
	err = pool.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM chunks WHERE version_id = $1),
		       (SELECT count(*) FROM chunk_embeddings_gemini_1536 e
		          JOIN chunks c ON c.id = e.chunk_id WHERE c.version_id = $1)`,
		result.VersionID).Scan(&chunks, &vectors)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if chunks != vectors {
		t.Errorf("%d chunks but %d vectors — the difference is unretrievable", chunks, vectors)
	}
	if chunks != result.Chunks {
		t.Errorf("reported %d chunks, found %d", result.Chunks, chunks)
	}
}

// The same bytes must not enter the corpus twice under two identities. This is
// the case a (document_id, source_checksum) unique would have missed: the
// Ministry edits a law page's title between crawls, the document lookup misses,
// and a second document row is created for the identical PDF.
func TestTheSameFileUnderADifferentTitleIsStillRefused(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	svc := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger())
	sum := checksum(t)

	if _, err := svc.Ingest(ctx, uniqueRequest(t, sum)); err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}

	// Same bytes, different title — a retitled law page.
	renamed := uniqueRequest(t, sum)
	result, err := svc.Ingest(ctx, renamed)
	if err != nil {
		t.Fatalf("second Ingest() = %v", err)
	}
	if !result.AlreadyIngested {
		t.Error("the same PDF was ingested again under a new title, doubling that law " +
			"in every retrieval result")
	}
}

// Drafts must be invisible to retrieval, and the database must physically
// refuse two live versions of one document.
func TestPublishIfNoneLiveNeverDemotesALiveVersion(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	svc := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger())

	first := uniqueRequest(t, checksum(t))
	a, err := svc.Ingest(ctx, first)
	if err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}
	if !a.Published {
		t.Fatal("a document with nothing live was not published")
	}

	// A second edition of the same document, arriving later.
	second := uniqueRequest(t, checksum(t))
	second.Document = first.Document
	second.Version.Label = "2024"

	b, err := svc.Ingest(ctx, second)
	if err != nil {
		t.Fatalf("second Ingest() = %v", err)
	}
	if b.Published {
		t.Error("the second edition published itself, silently superseding the first")
	}
	if b.DocumentID != a.DocumentID {
		t.Errorf("the second edition created a new document (%s vs %s) instead of a version",
			b.DocumentID, a.DocumentID)
	}

	var live int
	if err := pool.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_versions WHERE document_id = $1 AND status = 'published'`,
		a.DocumentID).Scan(&live); err != nil {
		t.Fatalf("count live versions: %v", err)
	}
	if live != 1 {
		t.Errorf("%d published versions of one document, want exactly 1", live)
	}
}

// A conflict must say which constraint fired. With two uniques on
// document_versions, reporting a checksum collision as a label collision sends
// an operator looking at entirely the wrong thing.
func TestALabelConflictIsReportedAsALabelConflict(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	svc := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger())

	first := uniqueRequest(t, checksum(t))
	if _, err := svc.Ingest(ctx, first); err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}

	// Different bytes, same document, same label.
	clash := uniqueRequest(t, checksum(t))
	clash.Document = first.Document
	clash.Version.Label = first.Version.Label

	_, err := svc.Ingest(ctx, clash)
	if err == nil {
		t.Fatal("a duplicate version label was accepted")
	}
	if !errors.Is(err, legal.ErrDuplicateVersionLabel) {
		t.Errorf("Ingest() = %v, want ErrDuplicateVersionLabel", err)
	}
}

// Replace is the only sanctioned way to re-ingest after the extractor improves.
func TestReplaceSwapsTheVersionAndLeavesNoOrphans(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	svc := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger())
	sum := checksum(t)

	first, err := svc.Ingest(ctx, uniqueRequest(t, sum))
	if err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}

	again := uniqueRequest(t, sum)
	again.Replace = true
	second, err := svc.Ingest(ctx, again)
	if err != nil {
		t.Fatalf("replace Ingest() = %v", err)
	}
	if !second.Replaced {
		t.Error("replace did not report replacing the previous version")
	}
	if second.VersionID == first.VersionID {
		t.Error("replace reused the old version id")
	}

	// ON DELETE CASCADE should have taken the old version's chunks and vectors.
	var orphans int
	if err := pool.Pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE version_id = $1`, first.VersionID).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d chunks survived the replaced version", orphans)
	}
}

// An empty title would make the second such document attach to the first, via
// the (lower(title), language) lookup — merging two unrelated laws.
func TestAnEmptyTitleIsRefusedBeforeAnythingIsEmbedded(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	embedder := &countingEmbedder{}
	svc := ingestion.NewService(pool.Pool, embedder, testLogger())

	req := uniqueRequest(t, checksum(t))
	req.Document.Title = ""

	if _, err := svc.Ingest(ctx, req); err == nil {
		t.Fatal("an empty title was accepted")
	}
	if embedder.texts != 0 {
		t.Errorf("embedded %d texts before rejecting an invalid request", embedder.texts)
	}
}

// The failure that -replace has to survive.
//
// Replacing deletes a version to make room. If that delete happens before
// EmbedAll — ten minutes of metered network I/O that fails for ordinary
// reasons — then a quota ceiling or a Ctrl-C leaves the old version gone and
// nothing in its place, and a law disappears from the corpus. The delete
// therefore belongs inside the write transaction, and this is the test that
// says so.
func TestAFailedReplaceLeavesTheOriginalVersionIntact(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	sum := checksum(t)
	req := uniqueRequest(t, sum)

	first, err := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger()).Ingest(ctx, req)
	if err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}

	var before int
	if err := pool.Pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE version_id = $1`, first.VersionID).Scan(&before); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if before == 0 {
		t.Fatal("the original version has no chunks to lose")
	}

	// Now replace it, with an embedder that gives up part-way.
	failing := ingestion.NewService(pool.Pool, &countingEmbedder{failAfter: 1}, testLogger())
	again := uniqueRequest(t, sum)
	again.Document = req.Document
	again.Replace = true

	if _, err := failing.Ingest(ctx, again); err == nil {
		t.Fatal("the failing embedder was not reported")
	}

	var live int
	if err := pool.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_versions WHERE id = $1`, first.VersionID).Scan(&live); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if live != 1 {
		t.Fatal("the original version was deleted by a replace that then failed — " +
			"this law is now absent from the corpus")
	}

	var after int
	if err := pool.Pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE version_id = $1`, first.VersionID).Scan(&after); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if after != before {
		t.Errorf("the original version has %d chunks, had %d", after, before)
	}
}

// Replacing reuses the label the old version held, which must not be mistaken
// for a conflict with itself.
func TestReplaceKeepsTheSameVersionLabel(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	svc := ingestion.NewService(pool.Pool, &countingEmbedder{}, testLogger())
	sum := checksum(t)
	req := uniqueRequest(t, sum)

	if _, err := svc.Ingest(ctx, req); err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}

	again := uniqueRequest(t, sum)
	again.Document = req.Document
	again.Version.Label = req.Version.Label // the same edition, re-parsed
	again.Replace = true

	result, err := svc.Ingest(ctx, again)
	if err != nil {
		t.Fatalf("replace with the same label = %v; the version being replaced "+
			"must not count as a conflict with itself", err)
	}
	if !result.Replaced {
		t.Error("replace did not report replacing anything")
	}

	var versions int
	if err := pool.Pool.QueryRow(ctx,
		`SELECT count(*) FROM document_versions WHERE document_id = $1`,
		result.DocumentID).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versions != 1 {
		t.Errorf("%d versions after a replace, want 1", versions)
	}
}
