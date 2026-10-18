package corpus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Ingester is the slice of ingestion this driver needs.
type Ingester interface {
	Ingest(ctx context.Context, req ingestion.Request) (ingestion.Result, error)
}

// Options configure one ingest run.
type Options struct {
	// Limit stops after this many documents are attempted. Zero means all.
	Limit int

	// DryRun evaluates and reports without writing anything or embedding
	// anything. The safe first command, matching the crawler's own discover and
	// resolve.
	DryRun bool

	// NoPublish forces every document to a draft regardless of its gates.
	NoPublish bool

	// Replace re-ingests documents already in the corpus, deleting the version
	// their bytes previously produced. For re-running after the extractor
	// improves. Destructive, so it is never the default.
	Replace bool

	// Deny is a set of document ids never to ingest.
	//
	// No mechanical gate catches a document that parses perfectly and does not
	// belong — docs/CORPUS.md records a US Sentencing Commission manual sitting
	// in the corpus folder, which would answer Ethiopian sentencing questions
	// with American guidelines and cite them as authority. That judgement is a
	// human's, and this is where it is recorded.
	Deny map[string]bool

	Thresholds Thresholds
}

// Summary counts a run.
type Summary struct {
	Attempted   int
	Published   int
	Drafted     int
	Skipped     int
	Quarantined int
	Failed      int
	Duration    time.Duration
}

// Driver ingests an acquired corpus.
type Driver struct {
	source   Source
	ledger   *Ledger
	ingester Ingester
	log      *slog.Logger
}

func NewDriver(source Source, ledger *Ledger, ingester Ingester, log *slog.Logger) *Driver {
	return &Driver{source: source, ledger: ledger, ingester: ingester, log: log}
}

// Run ingests every acquired document that is ready for it.
//
// Sequential by design. The embedding provider's per-minute budget is shared
// and enforced process-wide, so running documents concurrently would not make
// the run faster — it would only make quota rejections more likely, and it
// would reintroduce the race the in-transaction publish decision exists to
// close.
func (d *Driver) Run(ctx context.Context, opts Options) (Summary, error) {
	start := time.Now()
	var summary Summary

	// A real run without an ingester would nil-panic part-way through the
	// corpus, after the report had already claimed progress. Caught here.
	if d.ingester == nil && !opts.DryRun {
		return summary, errors.New("no ingester: a run that is not a dry run needs a database")
	}

	records, err := d.source.Records()
	if err != nil {
		return summary, err
	}
	records = ordered(records)

	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			// Ctrl-C mid-run: everything already written stays, and the ledger
			// means the next run picks up from here.
			summary.Duration = time.Since(start)
			return summary, err
		}
		if opts.Limit > 0 && summary.Attempted >= opts.Limit {
			break
		}

		outcome, counted := d.process(ctx, rec, opts)
		if !counted {
			continue
		}
		summary.Attempted++

		switch outcome.Status {
		case StatusPublished:
			summary.Published++
		case StatusDraft:
			summary.Drafted++
		case StatusSkipped:
			summary.Skipped++
		case StatusQuarantine:
			summary.Quarantined++
		case StatusFailed:
			summary.Failed++
		}

		if !opts.DryRun {
			if err := d.ledger.Save(outcome); err != nil {
				return summary, err
			}
		}
	}

	summary.Duration = time.Since(start)
	return summary, nil
}

// process handles one document. The bool reports whether it was attempted at
// all — documents the crawler never finished are not this run's business and
// are not counted as skips.
func (d *Driver) process(ctx context.Context, rec crawler.Record, opts Options) (Outcome, bool) {
	log := d.log.With(slog.String("doc_id", rec.DocID))
	out := Outcome{DocID: rec.DocID, Checksum: rec.Checksum}

	// Not acquired. Silent, because a crawl in progress is normal.
	if rec.State != crawler.StateReady {
		return Outcome{}, false
	}

	// The crawler sets DuplicateOf *and* leaves the state ready, so this has to
	// be checked explicitly. The Ministry links the same consolidated PDF from
	// more than one law page; ingesting both would file every article under two
	// document identities and double that law in every retrieval result.
	if rec.DuplicateOf != "" {
		out.Status, out.Reason = StatusSkipped, "same PDF as "+rec.DuplicateOf
		log.InfoContext(ctx, "skipped: duplicate PDF", slog.String("duplicate_of", rec.DuplicateOf))
		return out, true
	}

	if opts.Deny[rec.DocID] {
		out.Status, out.Reason = StatusQuarantine, "on the denylist"
		log.WarnContext(ctx, "skipped: denylisted")
		return out, true
	}

	// Already done. The ledger answers this without a database round trip, and
	// without it a second run would re-read and re-plan every document.
	if !opts.Replace {
		if prev, found, err := d.ledger.Load(rec.DocID); err == nil && found &&
			prev.Status.Terminal() && prev.Checksum == rec.Checksum {
			return Outcome{}, false
		}
	}

	// Not extracted yet. Reported rather than silent: this is the step an
	// operator forgets, and "nothing happened" is an unhelpful way to find out.
	path := d.source.StructuredPath(rec.DocID)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		out.Status, out.Reason = StatusSkipped, "not extracted yet — run workers/extraction/extract.py"
		log.InfoContext(ctx, "skipped: no extraction", slog.String("path", path))
		return out, true
	}
	if err != nil {
		out.Status, out.Error = StatusFailed, err.Error()
		log.ErrorContext(ctx, "FAILED to open extraction", slog.Any("error", err))
		return out, true
	}
	defer func() { _ = file.Close() }()

	ext, err := ingestion.ParseExtraction(file)
	if err != nil {
		// A malformed extraction is quarantined, not retried: it is a parse
		// problem, and running it again produces the same bytes.
		out.Status, out.Error = StatusQuarantine, err.Error()
		log.WarnContext(ctx, "quarantined: extraction did not validate", slog.Any("error", err))
		return out, true
	}

	req, err := PlanRequest(rec, ext)
	if err != nil {
		out.Status, out.Error = StatusQuarantine, err.Error()
		log.WarnContext(ctx, "quarantined: cannot build a request", slog.Any("error", err))
		return out, true
	}

	verdict := Evaluate(rec, ext, opts.Thresholds)
	out.Reason = verdict.Reason()

	req.PublishPolicy = ingestion.PublishNever
	if verdict.OK() && !opts.NoPublish {
		// Not PublishAlways: the service decides inside its transaction whether
		// this document already has something live, so an older edition
		// ingested after a newer one cannot demote it.
		req.PublishPolicy = ingestion.PublishIfNoneLive
	}
	req.Replace = opts.Replace

	if opts.DryRun {
		out.Status = StatusDraft
		if verdict.OK() && !opts.NoPublish {
			out.Status = StatusPublished
		}
		log.InfoContext(ctx, "would ingest",
			slog.String("title", req.Document.Title),
			slog.String("version", req.Version.Label),
			slog.Int("articles", len(ext.Articles)),
			slog.String("outcome", string(out.Status)),
			slog.String("gates", verdict.Reason()))
		return out, true
	}

	result, err := d.ingester.Ingest(ctx, req)
	switch {
	case err == nil:
	case errors.Is(err, legal.ErrDuplicateVersionLabel):
		// The label is taken by different bytes. Never auto-resolved: inventing
		// a suffixed label would mint a citation that refers to nothing.
		out.Status, out.Reason = StatusQuarantine, err.Error()
		log.WarnContext(ctx, "quarantined: version label conflict", slog.Any("error", err))
		return out, true
	default:
		out.Status, out.Error = StatusFailed, err.Error()
		log.ErrorContext(ctx, "FAILED to ingest", slog.Any("error", err))
		return out, true
	}

	if result.AlreadyIngested {
		out.Status, out.Reason = StatusSkipped, "already in the corpus"
		log.InfoContext(ctx, "skipped: already ingested")
		return out, true
	}

	out.DocumentID = string(result.DocumentID)
	out.VersionID = string(result.VersionID)
	out.Articles = result.Articles
	out.Chunks = result.Chunks
	out.Status = StatusDraft
	if result.Published {
		out.Status = StatusPublished
	}

	log.InfoContext(ctx, "ingested",
		slog.String("title", req.Document.Title),
		slog.String("version", req.Version.Label),
		slog.Int("articles", result.Articles),
		slog.Int("chunks", result.Chunks),
		slog.Bool("published", result.Published),
		slog.String("gates", verdict.Reason()))

	return out, true
}

// ordered sorts documents oldest publication first, then by id.
//
// Deterministic because the publish rule is "first one wins": with an arbitrary
// order, which of two editions of a law ends up live would depend on the
// alphabetical order of Amharic slugs, which reads as a bug in the report.
// Oldest-first means a later edition arrives second and is correctly held as a
// draft for a human to promote.
func ordered(records []crawler.Record) []crawler.Record {
	out := make([]crawler.Record, len(records))
	copy(out, records)

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Page.PublicationDate, out[j].Page.PublicationDate
		switch {
		case a != nil && b != nil && !a.Equal(*b):
			return a.Before(*b)
		case a != nil && b == nil:
			return true // a dated document sorts before an undated one
		case a == nil && b != nil:
			return false
		}
		return out[i].DocID < out[j].DocID
	})
	return out
}

// Report renders a run's summary.
func (s Summary) Report() string {
	return fmt.Sprintf(`
ingest summary
  attempted    %d
  published    %d
  drafted      %d
  skipped      %d
  quarantined  %d
  failed       %d
  elapsed      %s
`, s.Attempted, s.Published, s.Drafted, s.Skipped, s.Quarantined, s.Failed,
		s.Duration.Round(time.Second))
}
