package corpus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// fakeSource is the store without the acquisition stack behind it, which is
// what the Source interface exists for.
type fakeSource struct {
	records []crawler.Record
	root    string
}

func (f *fakeSource) Records() ([]crawler.Record, error) { return f.records, nil }
func (f *fakeSource) StructuredPath(id string) string {
	return filepath.Join(f.root, "structured", id, "document.json")
}

// writeExtraction puts a document's JSON where the driver will look for it.
func (f *fakeSource) writeExtraction(t *testing.T, id string, ext ingestion.Extraction) {
	t.Helper()
	path := f.StructuredPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// countingIngester records what it was asked to do, and whether it was asked
// at all — the embedding provider sits behind it, so a call it should not have
// received is money spent for nothing.
type countingIngester struct {
	calls     []ingestion.Request
	err       error
	published bool
}

func (c *countingIngester) Ingest(_ context.Context, req ingestion.Request) (ingestion.Result, error) {
	c.calls = append(c.calls, req)
	if c.err != nil {
		return ingestion.Result{}, c.err
	}
	return ingestion.Result{
		DocumentID: "doc-1", VersionID: "ver-1",
		Articles: len(req.Extraction.Articles), Chunks: len(req.Extraction.Articles),
		Published: c.published && req.PublishPolicy != ingestion.PublishNever,
	}, nil
}

func newHarness(t *testing.T, records ...crawler.Record) (*Driver, *fakeSource, *countingIngester, *Ledger) {
	t.Helper()
	root := t.TempDir()
	src := &fakeSource{records: records, root: root}
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	ing := &countingIngester{published: true}
	return NewDriver(src, ledger, ing, slog.New(slog.DiscardHandler)), src, ing, ledger
}

func defaultOpts() Options { return Options{Thresholds: DefaultThresholds()} }

func TestCleanDocumentIsIngestedAndPublished(t *testing.T) {
	rec := record()
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if summary.Published != 1 {
		t.Fatalf("published %d documents, want 1 (%+v)", summary.Published, summary)
	}
	if len(ing.calls) != 1 {
		t.Fatalf("ingester called %d times, want 1", len(ing.calls))
	}
	// Never PublishAlways: the service decides inside its transaction whether
	// something is already live, so an older edition cannot demote a newer one.
	if got := ing.calls[0].PublishPolicy; got != ingestion.PublishIfNoneLive {
		t.Errorf("publish policy = %q, want %q", got, ingestion.PublishIfNoneLive)
	}
}

// The crawler sets DuplicateOf and leaves the state READY_FOR_INGESTION, so
// this has to be checked explicitly. The Ministry links the same consolidated
// PDF from more than one law page; ingesting both files every article under two
// document identities and doubles that law in every retrieval result.
func TestADuplicatePDFIsNeverIngested(t *testing.T) {
	rec := record()
	rec.DuplicateOf = "some-other-doc"
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(ing.calls) != 0 {
		t.Error("a duplicate PDF was ingested")
	}
	if summary.Skipped != 1 {
		t.Errorf("skipped %d, want 1", summary.Skipped)
	}
}

func TestAFailedGateStillIngestsButAsADraft(t *testing.T) {
	rec := record()
	rec.Page.Status = "Repealed"
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	// Still ingested: it belongs in the database and must be inspectable. Just
	// not visible to retrieval.
	if len(ing.calls) != 1 {
		t.Fatalf("ingester called %d times, want 1", len(ing.calls))
	}
	if got := ing.calls[0].PublishPolicy; got != ingestion.PublishNever {
		t.Errorf("publish policy = %q, want %q for a repealed law", got, ingestion.PublishNever)
	}
	if summary.Drafted != 1 {
		t.Errorf("drafted %d, want 1 (%+v)", summary.Drafted, summary)
	}
}

func TestDryRunWritesNothingAndEmbedsNothing(t *testing.T) {
	rec := record()
	d, src, ing, ledger := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	opts := defaultOpts()
	opts.DryRun = true
	if _, err := d.Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if len(ing.calls) != 0 {
		t.Error("a dry run called the ingester, which would have paid for embeddings")
	}
	if _, found, _ := ledger.Load(rec.DocID); found {
		t.Error("a dry run wrote a ledger entry")
	}
}

// Without this, a second run re-reads, re-plans and re-submits every document,
// and the only thing standing between that and a repeat embedding bill is a
// database round trip per document.
func TestASecondRunSkipsFinishedDocuments(t *testing.T) {
	rec := record()
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	if _, err := d.Run(context.Background(), defaultOpts()); err != nil {
		t.Fatalf("first Run() = %v", err)
	}
	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("second Run() = %v", err)
	}

	if len(ing.calls) != 1 {
		t.Errorf("ingester called %d times across two runs, want 1", len(ing.calls))
	}
	if summary.Attempted != 0 {
		t.Errorf("second run attempted %d documents, want 0", summary.Attempted)
	}
}

// A re-crawl that picks up a genuinely new edition must not be mistaken for
// work already done.
func TestChangedBytesAreIngestedAgain(t *testing.T) {
	rec := record()
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	if _, err := d.Run(context.Background(), defaultOpts()); err != nil {
		t.Fatalf("first Run() = %v", err)
	}

	src.records[0].Checksum = "a-different-sha256"
	if _, err := d.Run(context.Background(), defaultOpts()); err != nil {
		t.Fatalf("second Run() = %v", err)
	}

	if len(ing.calls) != 2 {
		t.Errorf("ingester called %d times, want 2 — new bytes are a new edition", len(ing.calls))
	}
}

func TestDocumentsWithoutAnExtractionAreReportedNotIngested(t *testing.T) {
	rec := record()
	d, _, ing, ledger := newHarness(t, rec) // no extraction written

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(ing.calls) != 0 {
		t.Error("ingested a document that was never extracted")
	}
	if summary.Skipped != 1 {
		t.Errorf("skipped %d, want 1", summary.Skipped)
	}

	// Reported rather than silent: forgetting extract.py is the common mistake,
	// and "nothing happened" is an unhelpful way to discover it.
	out, found, err := ledger.Load(rec.DocID)
	if err != nil || !found {
		t.Fatalf("no ledger entry: found=%v err=%v", found, err)
	}
	if out.Reason == "" {
		t.Error("ledger entry does not say why the document was skipped")
	}
}

func TestUnacquiredDocumentsAreLeftAlone(t *testing.T) {
	rec := record()
	rec.State = crawler.StatePDFDownload // crawl still in progress
	d, _, ing, _ := newHarness(t, rec)

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(ing.calls) != 0 || summary.Attempted != 0 {
		t.Error("touched a document the crawler had not finished")
	}
}

func TestDenylistedDocumentsAreQuarantined(t *testing.T) {
	rec := record()
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())

	opts := defaultOpts()
	opts.Deny = map[string]bool{rec.DocID: true}

	summary, err := d.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(ing.calls) != 0 {
		t.Error("ingested a denylisted document")
	}
	if summary.Quarantined != 1 {
		t.Errorf("quarantined %d, want 1", summary.Quarantined)
	}
}

// Inventing a suffixed label would mint a citation that looks authoritative and
// refers to nothing, so a conflict is a human's problem, not the driver's.
func TestAVersionLabelConflictIsQuarantinedNotResolved(t *testing.T) {
	rec := record()
	d, src, ing, _ := newHarness(t, rec)
	src.writeExtraction(t, rec.DocID, cleanExtraction())
	ing.err = legal.ErrDuplicateVersionLabel

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if summary.Quarantined != 1 {
		t.Errorf("quarantined %d, want 1 (%+v)", summary.Quarantined, summary)
	}
	if summary.Failed != 0 {
		t.Error("a label conflict was reported as a failure rather than a conflict")
	}
}

func TestAlreadyIngestedIsASkipNotAFailure(t *testing.T) {
	rec := record()
	root := t.TempDir()
	src := &fakeSource{records: []crawler.Record{rec}, root: root}
	src.writeExtraction(t, rec.DocID, cleanExtraction())
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}

	ing := &alreadyIngested{}
	d := NewDriver(src, ledger, ing, slog.New(slog.DiscardHandler))

	summary, err := d.Run(context.Background(), defaultOpts())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if summary.Skipped != 1 || summary.Failed != 0 {
		t.Errorf("already-ingested reported as %+v, want one skip", summary)
	}
}

type alreadyIngested struct{}

func (alreadyIngested) Ingest(context.Context, ingestion.Request) (ingestion.Result, error) {
	return ingestion.Result{AlreadyIngested: true}, nil
}

func TestCancellationStopsTheRunAndKeepsWhatWasDone(t *testing.T) {
	recs := []crawler.Record{record(), record()}
	recs[1].DocID = "second-document"
	recs[1].Checksum = "another-checksum"

	d, src, _, _ := newHarness(t, recs...)
	for _, r := range recs {
		src.writeExtraction(t, r.DocID, cleanExtraction())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.Run(ctx, defaultOpts()); !errors.Is(err, context.Canceled) {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
}

func TestLimitStopsEarly(t *testing.T) {
	recs := []crawler.Record{record(), record(), record()}
	for i := range recs {
		recs[i].DocID = string(rune('a' + i))
		recs[i].Checksum = recs[i].DocID + "-sum"
	}
	d, src, ing, _ := newHarness(t, recs...)
	for _, r := range recs {
		src.writeExtraction(t, r.DocID, cleanExtraction())
	}

	opts := defaultOpts()
	opts.Limit = 2
	if _, err := d.Run(context.Background(), opts); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if len(ing.calls) != 2 {
		t.Errorf("ingester called %d times, want 2", len(ing.calls))
	}
}

// -dry-run opens neither the database nor the embedding provider, so the
// driver is handed no ingester. A real run in that state would nil-panic
// part-way through the corpus, after the report had claimed progress.
func TestARealRunWithoutAnIngesterIsRefusedUpFront(t *testing.T) {
	root := t.TempDir()
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDriver(&fakeSource{records: []crawler.Record{record()}, root: root},
		ledger, nil, slog.New(slog.DiscardHandler))

	if _, err := d.Run(context.Background(), defaultOpts()); err == nil {
		t.Error("a real run with no ingester was allowed to start")
	}
}

func TestDryRunWorksWithoutAnIngester(t *testing.T) {
	root := t.TempDir()
	src := &fakeSource{records: []crawler.Record{record()}, root: root}
	src.writeExtraction(t, record().DocID, cleanExtraction())
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}

	opts := defaultOpts()
	opts.DryRun = true
	summary, err := NewDriver(src, ledger, nil, slog.New(slog.DiscardHandler)).Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry Run() = %v", err)
	}
	if summary.Attempted != 1 {
		t.Errorf("attempted %d, want 1", summary.Attempted)
	}
}
