package crawler

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// Download is what came back from a successful fetch.
type Download struct {
	// Candidate is the link that actually served the file, so the log and the
	// stored metadata record which of a page's several links was the real one.
	Candidate Candidate

	Path     string
	Checksum string // sha256, lowercase hex
	Bytes    int64
}

// Downloader fetches the PDF behind a law page.
type Downloader struct {
	client *Client
	log    *slog.Logger
}

func NewDownloader(client *Client, log *slog.Logger) *Downloader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Downloader{client: client, log: log}
}

// ErrNoPDF means no candidate on the page served a usable PDF.
var ErrNoPDF = errors.New("no usable PDF found")

// Fetch tries each candidate in order and saves the first that proves to be a
// PDF, returning which one won.
//
// Trying rather than choosing is the design. The Ministry's pages carry a
// JetElements button that looks authoritative and answers HTTP 500 more often
// than not, so "pick the best-looking link and fail if it breaks" loses
// documents whose real PDF was sitting in the next element. A candidate that
// fails is logged and the walk continues; only exhausting the list is a failure.
//
// Validation happens on the first bytes, before the body is written to disk, so
// an HTML error page never lands in the corpus directory at all.
func (d *Downloader) Fetch(ctx context.Context, candidates []Candidate, destPath string) (Download, error) {
	if len(candidates) == 0 {
		return Download{}, fmt.Errorf("%w: page had no candidate links", ErrNoPDF)
	}

	var problems []error
	for _, c := range candidates {
		if c.Source == SourceJetDownload {
			d.log.InfoContext(ctx, "JET_DOWNLOAD_CANDIDATE", slog.String("url", c.URL))
		} else {
			d.log.InfoContext(ctx, "PDF_CANDIDATE",
				slog.String("url", c.Display),
				slog.String("source", string(c.Source)))
		}

		dl, err := d.fetchOne(ctx, c, destPath)
		if err == nil {
			d.log.InfoContext(ctx, "PDF_LINK_SELECTED",
				slog.String("url", dl.Candidate.Display),
				slog.String("source", string(dl.Candidate.Source)))
			d.log.InfoContext(ctx, "PDF_DOWNLOADED",
				slog.String("path", dl.Path),
				slog.Int64("bytes", dl.Bytes),
				slog.String("sha256", dl.Checksum))
			return dl, nil
		}

		if c.Source == SourceJetDownload {
			// The expected case, called out by name: this endpoint failing is
			// not a document failing, and the log should not read as though it
			// were.
			d.log.WarnContext(ctx, "JET_DOWNLOAD_FAILED",
				slog.String("url", c.URL),
				slog.Any("error", err))
		} else {
			d.log.WarnContext(ctx, "PDF_CANDIDATE_REJECTED",
				slog.String("url", c.Display),
				slog.String("source", string(c.Source)),
				slog.Any("error", err))
		}
		problems = append(problems, fmt.Errorf("%s (%s): %w", c.Display, c.Source, err))
	}

	return Download{}, fmt.Errorf("%w after %d candidate(s): %w",
		ErrNoPDF, len(candidates), errors.Join(problems...))
}

// fetchOne downloads and validates a single candidate.
func (d *Downloader) fetchOne(ctx context.Context, c Candidate, destPath string) (Download, error) {
	resp, err := d.client.Get(ctx, c.URL)
	if err != nil {
		return Download{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	// Buffered so the bytes read to identify the file are still there to be
	// written: a PDF's header is part of the PDF.
	body := bufio.NewReaderSize(resp.Body, peekSize)
	peek, err := body.Peek(peekSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return Download{}, fmt.Errorf("read response head: %w", err)
	}
	if err := ValidateResponse(resp, peek); err != nil {
		return Download{}, err
	}

	written, sum, err := writeAndHash(destPath, body)
	if err != nil {
		return Download{}, err
	}

	// Checked on what actually arrived, not on Content-Length: a transfer cut
	// short mid-stream still starts with %PDF-.
	if written < MinPDFSize {
		_ = os.Remove(destPath)
		return Download{}, fmt.Errorf("%w: %d bytes written is too small for a legal document",
			ErrNotPDF, written)
	}

	return Download{Candidate: c, Path: destPath, Checksum: sum, Bytes: written}, nil
}

// writeAndHash streams a body to disk, hashing as it goes.
//
// Written to a temporary file and renamed on success, so an interrupted crawl
// cannot leave a half-file that a later run mistakes for a complete document.
// Hashing in the same pass avoids reading tens of megabytes back off disk.
func writeAndHash(destPath string, src io.Reader) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, "", fmt.Errorf("create directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".partial-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once renamed
	}()

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), src)
	if err != nil {
		return 0, "", fmt.Errorf("write pdf: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, "", fmt.Errorf("close pdf: %w", err)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		return 0, "", fmt.Errorf("finalise pdf: %w", err)
	}

	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

// Probe reports whether a URL serves a PDF, without downloading it.
//
// Used by the dry-run mode, so an operator can check the resolver's choices
// across a sample of pages without pulling gigabytes. It asks for the first
// kilobyte; a server that ignores Range answers with the whole file, which is
// abandoned after the header is read.
func (d *Downloader) Probe(ctx context.Context, c Candidate) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", peekSize-1))

	if err := d.client.lim.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter: %w", err)
	}
	resp, err := d.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", c.Display, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, peekSize))
		_ = resp.Body.Close()
	}()

	peek, err := io.ReadAll(io.LimitReader(resp.Body, peekSize))
	if err != nil {
		return fmt.Errorf("read probe body: %w", err)
	}

	// A server honouring Range answers 206, which is a success here even though
	// ValidateResponse insists on 200 for a real download.
	if resp.StatusCode == http.StatusPartialContent {
		if !ValidatePDFHeader(peek) {
			return fmt.Errorf("%w: body does not start with %%PDF-%s", ErrNotPDF, describe(peek))
		}
		return nil
	}
	return ValidateResponse(resp, peek)
}
