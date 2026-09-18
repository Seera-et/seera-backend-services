// Command api is the Seera backend: one binary, one process.
//
// main does wiring and nothing else — config, logger, database, migrations,
// server, shutdown. If logic accumulates here, it belongs in a package.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/chat"
	"github.com/Seera-et/seera-backend-services/internal/citations"
	"github.com/Seera-et/seera-backend-services/internal/config"
	"github.com/Seera-et/seera-backend-services/internal/database"
	"github.com/Seera-et/seera-backend-services/internal/dialogue"
	"github.com/Seera-et/seera-backend-services/internal/documents"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/internal/qa"
	"github.com/Seera-et/seera-backend-services/internal/rag"
	"github.com/Seera-et/seera-backend-services/internal/router"
	"github.com/Seera-et/seera-backend-services/internal/search"
	"github.com/Seera-et/seera-backend-services/internal/server"
	"github.com/Seera-et/seera-backend-services/internal/tracing"
	"github.com/Seera-et/seera-backend-services/pkg/logger"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet, so report to stderr directly.
		slog.Error("startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(os.Stdout, cfg.LogLevel, cfg.IsDev())
	slog.SetDefault(log)

	log.Info("starting", slog.String("env", cfg.Env))
	if missing := cfg.MissingAIKeys(); len(missing) > 0 {
		// Not fatal: the phases that need these are not wired yet, and each
		// provider refuses to construct without its key. Visible at boot so the
		// gap is never discovered by a user's first question.
		log.Warn("AI credentials not configured", slog.Any("keys", missing))
	}

	// Cancelled on SIGINT/SIGTERM; every long-lived component watches it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.DatabaseURL.Value())
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("database connected")

	if err := database.Migrate(ctx, db, log); err != nil {
		return err
	}

	handlers, err := buildHandlers(cfg, log, db)
	if err != nil {
		return err
	}

	return server.New(cfg, log, db, handlers).Run(ctx)
}

// buildHandlers wires the feature handlers whose dependencies are configured.
//
// A feature whose credentials are missing is left unwired, and its route is not
// registered — a 404 is an honest answer, where a route that exists and fails
// on every call is not.
func buildHandlers(cfg config.Config, log *slog.Logger, db *database.Pool) (server.Handlers, error) {
	var handlers server.Handlers

	embedder, err := buildEmbedder(cfg, log)
	if err != nil {
		return server.Handlers{}, err
	}

	// Ingestion needs an embedder to index with and a token to guard the route.
	switch {
	case embedder == nil:
		log.Warn("ingestion disabled: no embedding provider configured")
	case cfg.AdminToken.Empty():
		log.Warn("ingestion disabled: ADMIN_API_TOKEN is not set")
	default:
		batch := ingestion.NewBatchEmbedder(embedder).
			PerMinute(cfg.EmbedTextsPerMinute).
			OnWait(
				func(attempt int, wait time.Duration, cause error) {
					// Distinguishes "waiting on the provider's quota window" from
					// "stuck": a large legal code on a free tier spends most of its
					// ingestion here, and silence looks identical to a hang.
					if attempt == 0 {
						// Deliberate pacing, not a rejection.
						log.Info("pacing embeddings to stay inside quota",
							slog.Duration("wait", wait.Round(time.Second)))
						return
					}
					log.Warn("embedding rate limited, waiting as instructed",
						slog.Int("attempt", attempt),
						slog.Duration("wait", wait),
						slog.String("cause", compactError(cause.Error())))
				})
		ingestSvc := ingestion.NewService(db.Pool, batch, log)
		handlers.Ingest = ingestion.NewHandler(ingestSvc, log)
		log.Info("ingestion enabled",
			slog.String("embedding_model", embedder.Model()),
			slog.Int("dimensions", embedder.Dimensions()))
	}

	// Citations resolve straight out of the corpus, so this endpoint works as
	// soon as there is a database — no AI credentials involved.
	citationSvc := citations.NewService(citations.NewRepository(db.Pool), log)
	handlers.Citation = citationSvc.Handler()

	// Traces record what a request decided (see internal/dialogue.Tracer) and
	// read back by request id. Needs only a database, same as citations — the
	// read endpoint is registered even if Q&A itself ends up disabled below;
	// it just never has anything to return in that case.
	tracer := tracing.NewRepository(db.Pool, log)
	handlers.Traces = tracer.Handler(log)

	// The catalogue reads over published versions and needs no provider, so
	// browsing and keyword search work on a database with a corpus and no API
	// keys at all. The embedder is passed when there is one and only adds the
	// semantic search arm: with it, a hybrid search fuses lexical and vector
	// results; without it, the same request is served as keyword-only and says
	// so in the response, rather than failing.
	documentsRepo, err := documents.NewRepository(db.Pool, documents.WithEmbedder(embedder))
	if err != nil {
		return server.Handlers{}, err
	}
	documentsHandler := documents.NewHandler(documents.NewService(documentsRepo), log)
	handlers.Documents = documentsHandler.Routes()
	handlers.CorpusStats = documentsHandler.Stats()
	if embedder == nil {
		log.Warn("corpus search limited to keyword matching: no embedding provider configured")
	}

	// Q&A needs retrieval (an embedder) and generation (an LLM). Either missing
	// leaves the route unregistered rather than serving something that fails on
	// every call.
	switch {
	case embedder == nil:
		log.Warn("Q&A disabled: no embedding provider configured")
	case cfg.GeminiAPIKey.Empty():
		log.Warn("Q&A disabled: GEMINI_API_KEY is not set")
	default:
		llmOpts := []ai.GeminiOption{}
		if cfg.GeminiModel != "" {
			llmOpts = append(llmOpts, ai.WithGeminiModel(cfg.GeminiModel))
		}
		geminiLLM, err := ai.NewGeminiLLM(cfg.GeminiAPIKey.Value(), llmOpts...)
		if err != nil {
			return server.Handlers{}, err
		}
		llm, err := buildLLM(cfg, log, geminiLLM)
		if err != nil {
			return server.Handlers{}, err
		}

		// WithTranslation needs llm, so search.NewService is built after it —
		// the only reason the two swapped order from before this existed.
		searchSvc, err := search.NewService(db.Pool, embedder, search.WithTranslation(llm, log))
		if err != nil {
			return server.Handlers{}, err
		}

		reranker, ragCfg := buildReranker(cfg, log, llm)
		ragCfg.MultiHop = cfg.MultiHop
		ragCfg.CitationEntailment = cfg.CitationEntailment

		legalOpts := []rag.Option{
			rag.WithSources(citationSvc),
			rag.WithArticles(searchSvc),
		}
		if cfg.CitationEntailment {
			// Experimental (see config.Config.CitationEntailment). The
			// verifier is only ever consulted when the flag is also set —
			// see rag.Service.Answer — so building it unconditionally here
			// would be dead weight, not a safety net.
			legalOpts = append(legalOpts, rag.WithVerifier(rag.NewLLMVerifier(llm)))
			log.Info("citation entailment enabled (experimental, adds a call after each answer)")
		}

		// The legal engine. It carries forward the sources a conversation has
		// already cited, and can look an article up by number when the second
		// retrieval pass asks for one.
		legalSvc := rag.NewService(searchSvc, reranker, llm, ragCfg, log, legalOpts...)

		// Cached in front of the engine, not inside it: a repeated or refreshed
		// question skips rerank and generation when retrieval confirms nothing
		// relevant has changed. See rag.Cache's doc comment for why this is safe
		// to leave always on rather than gated behind a flag.
		cachedLegal := rag.NewCache(legalSvc, searchSvc, ragCfg.Candidates, rag.DefaultCacheConfig(), log)

		// The branch point. Everything arrives here first: the router says what
		// the message is, and this hands it to the engine that should answer.
		answerSvc := dialogue.New(
			router.New(llm, log),
			chat.New(llm, log),
			cachedLegal,
			log,
			dialogue.WithPrefetch(searchSvc, ragCfg.Candidates),
			dialogue.WithSourceLabels(citationSvc),
			dialogue.WithTracer(tracer),
		)
		handlers.QA = qa.NewHandler(answerSvc, log)

		log.Info("Q&A enabled",
			slog.String("model", llm.Model()),
			slog.String("reranker", reranker.Name()),
			slog.Int("top_n", ragCfg.TopN),
			slog.Bool("multi_hop", ragCfg.MultiHop))
	}

	return handlers, nil
}

// buildEmbedder picks the embedding provider.
//
// Gemini is preferred when its key is present: the corpus is half Amharic, and
// Gemini's multilingual coverage is materially better than
// text-embedding-3-large's — which is the risk the plan flagged in §2.1 and
// reserved a migration for. Each provider writes to its own table, so both can be
// populated at once and compared on the language-split evaluation.
func buildEmbedder(cfg config.Config, log *slog.Logger) (ai.Embedder, error) {
	switch {
	case !cfg.GeminiAPIKey.Empty():
		return ai.NewGeminiEmbedder(cfg.GeminiAPIKey.Value())
	case !cfg.OpenAIAPIKey.Empty():
		log.Info("using OpenAI embeddings; set GEMINI_API_KEY for better Amharic coverage")
		return ai.NewOpenAIEmbedder(cfg.OpenAIAPIKey.Value())
	default:
		return nil, nil
	}
}

// buildLLM wraps the Gemini generation client with an OpenRouter fallback when
// OPENROUTER_API_KEY is set. Embeddings never go through this path — the
// corpus is embedded in Gemini's vector space specifically, and swapping that
// would silently mismatch every stored vector, not gracefully degrade one.
func buildLLM(cfg config.Config, log *slog.Logger, primary ai.LLM) (ai.LLM, error) {
	if cfg.OpenRouterAPIKey.Empty() {
		return primary, nil
	}

	opts := []ai.OpenRouterOption{}
	if cfg.OpenRouterModel != "" {
		opts = append(opts, ai.WithOpenRouterModel(cfg.OpenRouterModel))
	}
	fallback, err := ai.NewOpenRouterLLM(cfg.OpenRouterAPIKey.Value(), opts...)
	if err != nil {
		return nil, err
	}
	log.Info("OpenRouter fallback enabled", slog.String("model", fallback.Model()))
	return ai.NewFallbackLLM(primary, fallback, log), nil
}

// buildReranker picks the reranker and the abstention threshold that goes with it.
//
// The threshold is not portable between rerankers: Cohere returns a calibrated
// relevance score, the LLM reranker returns its own 0-1 judgment on a
// different (unvalidated) scale, and the passthrough returns an RRF value on
// an unrelated scale entirely. Setting a score threshold without a real
// reranker would abstain on everything, so a reranker and its threshold are
// always chosen together.
//
// llm is the already-built generation client (Gemini, or the OpenRouter
// fallback) — passed in rather than constructed here so this function has no
// credential of its own to fail on for the LLM reranker path.
func buildReranker(cfg config.Config, log *slog.Logger, llm ai.LLM) (rag.Reranker, rag.Config) {
	ragCfg := rag.DefaultConfig()

	if cfg.CohereAPIKey.Empty() {
		// Not PassthroughReranker: without a calibrated score, shouldAbstain's
		// only signal is "zero candidates" — a single, barely-relevant result
		// always passes. The LLM reranker costs no new credential (the model
		// already answering questions scores its own retrieval candidates) and
		// gives abstention a real number to compare against.
		log.Info("no COHERE_API_KEY: reranking with the LLM instead of a cross-encoder. " +
			"Slower and unvalidated — set COHERE_API_KEY for a purpose-built, calibrated reranker.")
		ragCfg.MinRerankScore = rag.LLMRerankMinScore
		return rag.NewLLMReranker(llm), ragCfg
	}

	reranker, err := rag.NewCohereReranker(cfg.CohereAPIKey.Value())
	if err != nil {
		log.Warn("Cohere reranker unavailable, using the LLM reranker instead", slog.Any("error", err))
		ragCfg.MinRerankScore = rag.LLMRerankMinScore
		return rag.NewLLMReranker(llm), ragCfg
	}

	// Only meaningful with a calibrated scorer. Below this, the top result is
	// not about the question and abstaining beats answering.
	ragCfg.MinRerankScore = 0.15
	return reranker, ragCfg
}

// compactError flattens a provider's multi-line error into one log field,
// keeping the part an operator can act on.
//
// Quota errors bury the useful sentence — which metric, what limit, how long to
// wait — behind boilerplate about billing and documentation URLs. Truncating
// from the front threw exactly that away, and the difference matters: a
// per-minute limit means wait, a daily limit means stop until tomorrow or enable
// billing.
func compactError(s string) string {
	s = strings.Join(strings.Fields(s), " ")

	if i := strings.Index(s, "Quota exceeded for metric"); i >= 0 {
		s = s[i:]
	}
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
