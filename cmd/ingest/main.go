// Command ingest moves an acquired corpus into the database.
//
// It is the bridge between the crawler, which stops at a validated PDF, and the
// corpus that answers questions. For each document the crawler acquired and
// workers/extraction parsed, it builds the ingestion request from the crawl
// record — the metadata was scraped once and should not be retyped — chunks,
// embeds, writes, and publishes the ones whose parse clears the quality gates.
//
//	ingest -data ./data -dry-run      # what would happen; writes and embeds nothing
//	ingest -data ./data               # the real thing
//	ingest -data ./data -replace      # re-ingest after improving the extractor
//
// It talks to Postgres directly rather than to the admin HTTP endpoint. A
// 600-article code is around ten minutes of embedding, which is a long time to
// hold one request open behind a proxy, and the endpoint caps bodies at
// MAX_INGEST_BYTES. The endpoint remains the right tool for a single document.
//
// main does wiring and nothing else; the work is in internal/corpus.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Seera-et/seera-backend-services/internal/config"
	"github.com/Seera-et/seera-backend-services/internal/corpus"
	"github.com/Seera-et/seera-backend-services/internal/database"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/pkg/logger"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			// Ctrl-C. Everything written so far stands, and the ledger lets the
			// next run resume — not a failure.
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "ingest: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dataDir   = flag.String("data", "./data", "crawler store to ingest from")
		limit     = flag.Int("limit", 0, "stop after this many documents (0 = no limit)")
		dryRun    = flag.Bool("dry-run", false, "report what would happen; write and embed nothing")
		noPublish = flag.Bool("no-publish", false, "ingest everything as a draft, whatever the gates say")
		replace   = flag.Bool("replace", false,
			"re-ingest documents already in the corpus, deleting the version their bytes produced")
		denyList = flag.String("deny", "", "file of document ids never to ingest, one per line")
		verbose  = flag.Bool("v", false, "debug logging")

		t             = corpus.DefaultThresholds()
		minCoverage   = flag.Float64("min-coverage", t.MinCoverage, "share of text that must fall inside an article")
		maxGapRatio   = flag.Float64("max-gap-ratio", t.MaxGapRatio, "missing article numbers allowed, per article found")
		maxBlankPages = flag.Float64("max-blank-pages", t.MaxPagesWithoutText, "share of pages allowed to have no text layer")
		minLangAgree  = flag.Float64("min-language-agreement", t.MinLanguageAgreement, "how strongly the script must match the declared language")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logLevel := cfg.LogLevel
	if *verbose {
		logLevel = "debug"
	}
	log := logger.New(os.Stdout, logLevel, cfg.IsDev())

	// Ctrl-C cancels in-flight work; the ledger makes the next run resume.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deny, err := loadDenyList(*denyList)
	if err != nil {
		return err
	}

	source, err := corpus.NewCrawlerSource(*dataDir)
	if err != nil {
		return err
	}
	ledger, err := corpus.OpenLedger(*dataDir)
	if err != nil {
		return err
	}

	// A dry run reads the store and evaluates gates; it touches neither the
	// database nor the embedding provider. So it does not open either — which
	// is what makes it usable as the first command, before credentials are in
	// place, the same way `crawler discover` is safe to run before a crawl.
	var ingester corpus.Ingester
	if !*dryRun {
		db, err := database.Connect(ctx, cfg.DatabaseURL.Value())
		if err != nil {
			return err
		}
		defer db.Close()
		log.Info("database connected")

		if err := database.Migrate(ctx, db, log); err != nil {
			return fmt.Errorf("%w\n\nThe schema is behind. Boot the API against this "+
				"database once to migrate it: DATABASE_URL=... go run ./cmd/api", err)
		}

		embedder, err := ingestion.NewEmbedder(cfg.GeminiAPIKey.Value(), cfg.OpenAIAPIKey.Value(), log)
		if err != nil {
			return err
		}
		if embedder == nil {
			return errors.New("no embedding provider configured: " +
				"set GEMINI_API_KEY (preferred) or OPENAI_API_KEY")
		}
		log.Info("embedding with",
			slog.String("model", embedder.Model()),
			slog.Int("dimensions", embedder.Dimensions()),
			slog.Int("texts_per_minute", cfg.EmbedTextsPerMinute))

		ingester = ingestion.NewService(db.Pool,
			ingestion.Paced(embedder, cfg.EmbedTextsPerMinute, log), log)
	}

	summary, runErr := corpus.NewDriver(source, ledger, ingester, log).Run(ctx, corpus.Options{
		Limit:     *limit,
		DryRun:    *dryRun,
		NoPublish: *noPublish,
		Replace:   *replace,
		Deny:      deny,
		Thresholds: corpus.Thresholds{
			MinCoverage:          *minCoverage,
			MaxGapRatio:          *maxGapRatio,
			MaxPagesWithoutText:  *maxBlankPages,
			MinLanguageAgreement: *minLangAgree,
		},
	})

	// Printed even on error: a run interrupted after forty documents did forty
	// documents of work, and the operator needs the count either way.
	fmt.Fprint(os.Stderr, summary.Report())
	if *dryRun {
		fmt.Fprintln(os.Stderr, "  (dry run — nothing was written)")
	}

	return runErr
}

// loadDenyList reads document ids that must never be ingested, one per line,
// ignoring blanks and # comments.
//
// This exists because no mechanical gate can catch a document that parses
// perfectly and does not belong: docs/CORPUS.md records a US Sentencing
// Commission manual in the corpus folder, which would answer Ethiopian
// sentencing questions with American guidelines and cite them as authority.
func loadDenyList(path string) (map[string]bool, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read denylist: %w", err)
	}

	deny := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		deny[line] = true
	}
	return deny, nil
}
