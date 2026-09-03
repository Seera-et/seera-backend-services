// Command crawler acquires Ethiopian legal documents from justice.gov.et.
//
// It is deliberately a separate binary from cmd/api. Acquisition is an
// operator-run, occasionally-run job measured in hours, with completely
// different failure modes from serving questions — and nothing about the API
// should be able to start a crawl.
//
//	crawler discover -limit 20
//	crawler resolve  -limit 20
//	crawler run      -limit 50 -data ./data
//
// Then, offline:
//
//	python workers/extraction/triage.py ./data/raw
//	python workers/extraction/extract.py ./data/raw/<id>/original.pdf --out ./data/structured/<id>/
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "crawler: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("no command given")
	}

	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)

	var (
		dataDir     = flags.String("data", "./data", "directory for downloaded PDFs and metadata")
		limit       = flags.Int("limit", 0, "stop after this many law pages (0 = no limit)")
		maxPages    = flags.Int("max-pages", 0, "cap pagination per listing (0 = no cap)")
		concurrency = flags.Int("concurrency", 2, "documents processed at once")
		rps         = flags.Float64("rps", crawler.DefaultClientConfig().RequestsPerSecond,
			"requests per second ceiling; raising this is rude to a government site")
		indexPaths = flags.String("listings", strings.Join(crawler.DefaultIndexPaths, ","),
			"comma-separated listing paths to walk")
		verbose = flags.Bool("v", false, "debug logging")
		resume  = flags.Bool("resume", true, "skip documents already finished in a previous run")
		refetch = flags.Bool("refetch", false, "re-download even when a PDF is already on disk")
	)
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Ctrl-C stops politely: in-flight requests are cancelled, and every record
	// written so far stays on disk for -resume to pick up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := crawler.DefaultClientConfig()
	cfg.RequestsPerSecond = *rps
	client := crawler.NewClient(cfg, log)

	discoverOpts := crawler.DiscoverOptions{
		IndexPaths: splitPaths(*indexPaths),
		MaxPages:   *maxPages,
		Limit:      *limit,
	}

	switch command {
	case "discover":
		return discover(ctx, client, log, discoverOpts)
	case "resolve":
		return resolve(ctx, client, log, discoverOpts)
	case "run":
		return crawl(ctx, client, log, *dataDir, crawler.Options{
			Discover:    discoverOpts,
			Concurrency: *concurrency,
			Resume:      *resume,
			Refetch:     *refetch,
		})
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `crawler — acquire Ethiopian legal documents from justice.gov.et

  discover   list the law pages found, and stop. Downloads nothing.
  resolve    for each law page, report which link would be downloaded.
             Fetches page HTML and probes candidates; downloads no PDFs.
  run        the full pipeline: discover, resolve, download, validate, record.

Run with -h after a command for its flags.
`)
}

func splitPaths(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// discover lists what a crawl would visit. The safe first command to run
// against the Ministry: it reads listing pages only.
func discover(ctx context.Context, client *crawler.Client, log *slog.Logger, opts crawler.DiscoverOptions) error {
	d, err := crawler.NewDiscoverer(client, log)
	if err != nil {
		return err
	}

	refs, err := d.Discover(ctx, opts)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(refs); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n%d law pages discovered\n", len(refs))
	return nil
}

// resolve reports the download decision per page without downloading anything.
//
// This is the command that proves the resolver against the live site: it shows
// which candidates each page offered and which one won, so a JetElements button
// beating the real link would be visible before gigabytes are pulled.
func resolve(ctx context.Context, client *crawler.Client, log *slog.Logger, opts crawler.DiscoverOptions) error {
	d, err := crawler.NewDiscoverer(client, log)
	if err != nil {
		return err
	}
	fetcher := crawler.NewFetcher(client, log)
	downloader := crawler.NewDownloader(client, log)

	refs, err := d.Discover(ctx, opts)
	if err != nil {
		return err
	}

	var withPDF, withoutPDF int
	for _, ref := range refs {
		page, err := fetcher.Fetch(ctx, ref)
		if err != nil {
			fmt.Printf("%-50s  FETCH FAILED: %v\n", ref.Slug, err)
			continue
		}
		if len(page.Candidates) == 0 {
			fmt.Printf("%-50s  NO PDF LINK\n", ref.Slug)
			withoutPDF++
			continue
		}

		var chosen bool
		for _, c := range page.Candidates {
			if err := downloader.Probe(ctx, c); err != nil {
				fmt.Printf("%-50s  reject [%s] %s: %v\n", ref.Slug, c.Source, c.Display, err)
				continue
			}
			fmt.Printf("%-50s  SELECT [%s] %s\n", ref.Slug, c.Source, c.Display)
			chosen = true
			break
		}
		if chosen {
			withPDF++
		} else {
			withoutPDF++
		}
	}

	fmt.Fprintf(os.Stderr, "\n%d resolved, %d without a usable PDF\n", withPDF, withoutPDF)
	return nil
}

func crawl(ctx context.Context, client *crawler.Client, log *slog.Logger, dataDir string, opts crawler.Options) error {
	store, err := crawler.NewStore(dataDir)
	if err != nil {
		return err
	}
	c, err := crawler.New(client, store, log)
	if err != nil {
		return err
	}

	summary, err := c.Run(ctx, opts)

	// Printed even on error: a crawl interrupted after 400 documents did 400
	// documents of useful work, and the operator needs the count either way.
	fmt.Fprintf(os.Stderr, `
crawl summary
  discovered  %d
  downloaded  %d
  duplicates  %d
  skipped     %d
  failed      %d
  elapsed     %s
`, summary.Discovered, summary.Downloaded, summary.Duplicates,
		summary.Skipped, summary.Failed, summary.Duration.Round(time.Second))

	return err
}
