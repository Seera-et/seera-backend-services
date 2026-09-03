package crawler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Options configure one crawl.
type Options struct {
	Discover DiscoverOptions

	// Concurrency is how many documents are processed at once. The rate
	// limiter, not this, governs how hard the site is hit; this only decides
	// how many documents are in flight while waiting for it.
	Concurrency int

	// Resume skips documents whose stored record already reached a terminal
	// state. On by default in practice: a four-figure crawl will be
	// interrupted, and re-downloading what already succeeded is rude to the
	// Ministry and slow for us.
	Resume bool

	// Refetch re-downloads even when a PDF is already on disk. For recovering
	// from a bad run, not for normal use.
	Refetch bool
}

// Summary counts what a crawl did.
type Summary struct {
	Discovered int
	Skipped    int
	Downloaded int
	Duplicates int
	Failed     int
	Duration   time.Duration
}

// Crawler runs the acquisition pipeline: discover, fetch, resolve, download.
//
// It stops at a validated PDF plus a metadata record. Text extraction, OCR and
// structure detection are workers/extraction's job — see the package comment.
type Crawler struct {
	discoverer *Discoverer
	fetcher    *Fetcher
	downloader *Downloader
	store      *Store
	log        *slog.Logger
}

func New(client *Client, store *Store, log *slog.Logger) (*Crawler, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	discoverer, err := NewDiscoverer(client, log)
	if err != nil {
		return nil, err
	}
	return &Crawler{
		discoverer: discoverer,
		fetcher:    NewFetcher(client, log),
		downloader: NewDownloader(client, log),
		store:      store,
		log:        log,
	}, nil
}

// Run discovers law pages and acquires each one's PDF.
//
// A document that fails is recorded and the crawl continues. At this scale an
// aborting crawl is useless: some proportion of a government site's documents
// will always be missing, mislinked or scanned, and the run has to report that
// rather than stop at the first one.
func (c *Crawler) Run(ctx context.Context, opts Options) (Summary, error) {
	start := time.Now()

	refs, err := c.discoverer.Discover(ctx, opts.Discover)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{Discovered: len(refs)}
	c.log.InfoContext(ctx, "discovery complete", slog.Int("law_pages", len(refs)))

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		work = make(chan LawPageRef)
	)

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range work {
				outcome := c.process(ctx, ref, opts)
				mu.Lock()
				switch outcome {
				case outcomeSkipped:
					summary.Skipped++
				case outcomeDownloaded:
					summary.Downloaded++
				case outcomeDuplicate:
					summary.Duplicates++
				case outcomeFailed:
					summary.Failed++
				}
				mu.Unlock()
			}
		}()
	}

	for _, ref := range refs {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return summary, ctx.Err()
		case work <- ref:
		}
	}
	close(work)
	wg.Wait()

	if err := c.store.SaveChecksums(); err != nil {
		c.log.WarnContext(ctx, "could not persist the checksum index", slog.Any("error", err))
	}

	summary.Duration = time.Since(start)
	return summary, nil
}

type outcome int

const (
	outcomeSkipped outcome = iota
	outcomeDownloaded
	outcomeDuplicate
	outcomeFailed
)

// process takes one law page from discovered to a validated PDF on disk.
func (c *Crawler) process(ctx context.Context, ref LawPageRef, opts Options) outcome {
	// The slug is the law's Amharic title and cannot be a directory name as-is
	// — see SafeDocID. The slug itself stays on the record, via Page.Ref.
	docID := SafeDocID(ref.Slug)
	log := c.log.With(
		slog.String("doc_id", docID),
		slog.String("slug", ref.Slug),
		slog.String("url", ref.URL))

	if opts.Resume && !opts.Refetch {
		if existing, found, err := c.store.LoadRecord(docID); err == nil && found && existing.State.Terminal() {
			log.DebugContext(ctx, "skipping, already finished",
				slog.String("state", string(existing.State)))
			return outcomeSkipped
		}
	}

	record := Record{DocID: docID, State: StateDiscovered, Page: LawPage{Ref: ref}}

	page, err := c.fetcher.Fetch(ctx, ref)
	if err != nil {
		log.ErrorContext(ctx, "FAILED", slog.String("reason", string(ReasonPageFetchFailed)), slog.Any("error", err))
		record.Fail(ReasonPageFetchFailed, err)
		c.save(ctx, record)
		return outcomeFailed
	}
	record.Page = page
	record.State = StateFetchedPage

	if len(page.Candidates) == 0 {
		err := errors.New("law page carried no link that could be a PDF")
		log.WarnContext(ctx, "FAILED", slog.String("reason", string(ReasonNoPDFLink)))
		record.Fail(ReasonNoPDFLink, err)
		c.save(ctx, record)
		return outcomeFailed
	}
	record.State = StatePDFFound

	download, err := c.downloader.Fetch(ctx, page.Candidates, c.store.PDFPath(docID))
	if err != nil {
		reason := classifyDownloadFailure(page.Candidates, err)
		log.ErrorContext(ctx, "FAILED", slog.String("reason", string(reason)), slog.Any("error", err))
		record.Fail(reason, err)
		c.save(ctx, record)
		return outcomeFailed
	}

	record.State = StatePDFValid
	record.OriginalFileURL = download.Candidate.URL
	record.PDFSource = string(download.Candidate.Source)
	record.Checksum = download.Checksum
	record.Bytes = download.Bytes

	result := outcomeDownloaded
	if owner, duplicate := c.store.ClaimChecksum(download.Checksum, docID); duplicate {
		// Kept on disk, marked, and not queued for ingestion: the same PDF
		// indexed twice would have every article cited under two documents.
		record.DuplicateOf = owner
		log.InfoContext(ctx, "duplicate PDF, already held by another law page",
			slog.String("sha256", download.Checksum),
			slog.String("duplicate_of", owner))
		result = outcomeDuplicate
	}

	// Ready for workers/extraction, which advances the record from here.
	record.State = StateReady
	c.save(ctx, record)
	return result
}

// classifyDownloadFailure decides which failure category to record.
//
// The distinction that matters is whether the Ministry's own broken
// JetElements button was the only thing on offer, which is a site problem, or
// whether real links were tried and rejected, which is a document problem.
func classifyDownloadFailure(candidates []Candidate, err error) FailureReason {
	if errors.Is(err, ErrNotPDF) {
		return ReasonInvalidPDF
	}

	onlyJet := true
	for _, c := range candidates {
		if c.Source != SourceJetDownload {
			onlyJet = false
			break
		}
	}
	if onlyJet {
		return ReasonJetDownloadFailed
	}
	if errors.Is(err, ErrNoPDF) {
		return ReasonNoPDFLink
	}
	return ReasonDownloadFailed
}

func (c *Crawler) save(ctx context.Context, r Record) {
	if err := c.store.SaveRecord(r); err != nil {
		c.log.ErrorContext(ctx, "could not write record",
			slog.String("doc_id", r.DocID), slog.Any("error", err))
	}
}
