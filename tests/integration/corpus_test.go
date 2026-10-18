//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/corpus"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
)

// storeFixture builds a crawler store on disk, the way cmd/crawler and
// workers/extraction leave one.
type storeFixture struct{ root string }

func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"raw", "metadata", "structured", "index"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &storeFixture{root: root}
}

// record writes a crawl record exactly as internal/crawler serialises one.
func (s *storeFixture) record(t *testing.T, docID, title, number, status, sum string) {
	t.Helper()
	body := map[string]any{
		"doc_id":   docID,
		"state":    "READY_FOR_INGESTION",
		"checksum": sum,
		"page": map[string]any{
			"ref": map[string]any{
				"url": "https://justice.gov.et/en/law/" + docID, "slug": docID, "page_language": "en",
			},
			"title_en": title, "document_number": number,
			"doc_type": "proclamation", "status": status,
		},
		"updated_at": "2026-01-01T00:00:00Z",
	}
	s.writeJSON(t, filepath.Join(s.root, "metadata", docID+".json"), body)
}

// extraction writes what workers/extraction --store-root produces.
func (s *storeFixture) extraction(t *testing.T, docID string, articles, pages, blankPages int) {
	t.Helper()
	arts := make([]map[string]any, articles)
	for i := range arts {
		arts[i] = map[string]any{
			"article_no": fmt.Sprint(i + 1), "article_no_raw": fmt.Sprint(i + 1),
			"title": "Provision", "chapter": "", "section_path": []string{"Part I"},
			"text":    fmt.Sprintf("Article %d states a requirement that applies generally.", i+1),
			"page_no": 1, "char_start": i * 100, "char_end": i*100 + 90,
			"structure_confidence": "high",
		}
	}
	body := map[string]any{
		"source_file": "original.pdf", "language": "en", "page_count": pages,
		"articles": arts, "warnings": []string{},
		"stats": map[string]any{
			"characters": 10000, "article_characters": 9200,
			"pages_without_text_layer": blankPages, "pages_ocred": 0,
			"structure": "article", "text_quality": 0.93, "ethiopic_ratio": 0.0,
			"article_gaps": 0, "article_number_min": 1, "article_number_max": articles,
		},
	}
	s.writeJSON(t, filepath.Join(s.root, "structured", docID, "document.json"), body)
}

func (s *storeFixture) writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The whole bridge, against a real database: crawler store on disk → records
// read → metadata mapped → gates evaluated → chunked, embedded, written →
// published or held as a draft.
//
// Only the embedding provider is stubbed. Everything else — the store format,
// the mapping, the gates, the transaction, the constraints — is the real thing.
func TestTheBridgeRunsEndToEnd(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	fx := newStoreFixture(t)
	unique := fmt.Sprintf("%d", osGetpidNano())

	// A clean, in-force proclamation: should publish.
	fx.record(t, "clean", "Clean Proclamation "+unique, "1234/2021", "In Force",
		runChecksum(unique, 1))
	fx.extraction(t, "clean", 20, 40, 0)

	// Same quality, but repealed: must be ingested and NOT published.
	fx.record(t, "repealed", "Repealed Proclamation "+unique, "99/1998", "Repealed",
		runChecksum(unique, 2))
	fx.extraction(t, "repealed", 20, 40, 0)

	// A scan with a few incidental text pages: must not publish.
	fx.record(t, "scanned", "Scanned Code "+unique, "77/1960", "In Force",
		runChecksum(unique, 3))
	fx.extraction(t, "scanned", 5, 300, 295)

	source, err := corpus.NewCrawlerSource(fx.root)
	if err != nil {
		t.Fatalf("NewCrawlerSource: %v", err)
	}
	ledger, err := corpus.OpenLedger(fx.root)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}

	embedder := &countingEmbedder{}
	svc := ingestion.NewService(pool.Pool, embedder, testLogger())
	driver := corpus.NewDriver(source, ledger, svc, testLogger())

	summary, err := driver.Run(ctx, corpus.Options{Thresholds: corpus.DefaultThresholds()})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if summary.Published != 1 {
		t.Errorf("published %d, want 1 (%+v)", summary.Published, summary)
	}
	if summary.Drafted != 2 {
		t.Errorf("drafted %d, want 2 — repealed and scanned (%+v)", summary.Drafted, summary)
	}

	// The repealed law must be in the database and invisible to retrieval.
	var status string
	err = pool.Pool.QueryRow(ctx, `
		SELECT v.status FROM document_versions v
		JOIN legal_documents d ON d.id = v.document_id
		WHERE d.title = $1`, "Repealed Proclamation "+unique).Scan(&status)
	if err != nil {
		t.Fatalf("the repealed law was not ingested at all: %v", err)
	}
	if status != "draft" {
		t.Errorf("the repealed law is %q; it would be cited as current law", status)
	}

	// Provenance made it onto the version.
	var sum, url, key string
	err = pool.Pool.QueryRow(ctx, `
		SELECT coalesce(v.source_checksum,''), v.source_url, v.storage_key
		FROM document_versions v JOIN legal_documents d ON d.id = v.document_id
		WHERE d.title = $1`, "Clean Proclamation "+unique).Scan(&sum, &url, &key)
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if sum == "" || url == "" || key != "clean" {
		t.Errorf("provenance incomplete: checksum=%q url=%q storage_key=%q", sum, url, key)
	}

	// Every chunk of every ingested document has a vector.
	var chunks, vectors int
	err = pool.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM chunks c JOIN document_versions v ON v.id = c.version_id
		          JOIN legal_documents d ON d.id = v.document_id WHERE d.title LIKE $1),
		       (SELECT count(*) FROM chunk_embeddings_gemini_1536 e
		          JOIN chunks c ON c.id = e.chunk_id
		          JOIN document_versions v ON v.id = c.version_id
		          JOIN legal_documents d ON d.id = v.document_id WHERE d.title LIKE $1)`,
		"%"+unique).Scan(&chunks, &vectors)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if chunks == 0 {
		t.Fatal("no chunks were written")
	}
	if chunks != vectors {
		t.Errorf("%d chunks but %d vectors — the difference is unretrievable", chunks, vectors)
	}

	// A second run must be a no-op, and must not embed again.
	embedded := embedder.texts
	second, err := driver.Run(ctx, corpus.Options{Thresholds: corpus.DefaultThresholds()})
	if err != nil {
		t.Fatalf("second Run() = %v", err)
	}
	if second.Attempted != 0 {
		t.Errorf("second run attempted %d documents, want 0", second.Attempted)
	}
	if embedder.texts != embedded {
		t.Errorf("second run embedded %d more texts", embedder.texts-embedded)
	}
}

// Deleting the ledger must not make a second run re-embed: the corpus itself
// remembers, through the version checksum.
func TestTheDatabaseAloneStopsAReEmbed(t *testing.T) {
	pool := connect(t)
	ctx := context.Background()

	fx := newStoreFixture(t)
	unique := fmt.Sprintf("%d", osGetpidNano())
	fx.record(t, "solo", "Solo Proclamation "+unique, "500/2020", "In Force",
		runChecksum(unique, 1))
	fx.extraction(t, "solo", 12, 20, 0)

	source, _ := corpus.NewCrawlerSource(fx.root)
	ledger, _ := corpus.OpenLedger(fx.root)
	embedder := &countingEmbedder{}
	driver := corpus.NewDriver(source, ledger,
		ingestion.NewService(pool.Pool, embedder, testLogger()), testLogger())

	if _, err := driver.Run(ctx, corpus.Options{Thresholds: corpus.DefaultThresholds()}); err != nil {
		t.Fatalf("first Run() = %v", err)
	}
	embedded := embedder.texts
	if embedded == 0 {
		t.Fatal("nothing was embedded on the first run")
	}

	// Lose the ledger — a new machine, a cleared data directory.
	if err := os.RemoveAll(filepath.Join(fx.root, "ingest")); err != nil {
		t.Fatal(err)
	}
	ledger2, _ := corpus.OpenLedger(fx.root)
	driver2 := corpus.NewDriver(source, ledger2,
		ingestion.NewService(pool.Pool, embedder, testLogger()), testLogger())

	summary, err := driver2.Run(ctx, corpus.Options{Thresholds: corpus.DefaultThresholds()})
	if err != nil {
		t.Fatalf("second Run() = %v", err)
	}
	if summary.Skipped != 1 {
		t.Errorf("skipped %d, want 1 — the checksum should have caught it (%+v)", summary.Skipped, summary)
	}
	if embedder.texts != embedded {
		t.Errorf("re-embedded %d texts after the ledger was lost; the pre-flight "+
			"checksum lookup is what should have prevented that", embedder.texts-embedded)
	}
}

// runChecksum is a sha256-shaped string unique to this run and document.
//
// Fixed checksums would make a second run against the same database skip every
// document — correctly, since the corpus remembers bytes it has already
// ingested — and the test would be measuring that instead of what it means to.
func runChecksum(unique string, n int) string {
	return fmt.Sprintf("%064s", fmt.Sprintf("%s%02d", unique, n))
}

// osGetpidNano gives each run a value no other run shares, so tests can create
// documents without colliding on the (lower(title), language) unique.
func osGetpidNano() int64 { return time.Now().UnixNano() }
