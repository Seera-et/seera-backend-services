// Package server owns HTTP: the router, middleware, response helpers, and the
// process's listen/shutdown lifecycle.
//
// It holds no business logic. Feature packages provide handlers; this package
// mounts them.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/Seera-et/seera-backend-services/internal/config"
	"github.com/Seera-et/seera-backend-services/internal/database"
)

// healthChecker is the only part of the database this package needs. Feature
// packages take the pool directly; the server just asks whether it is alive.
// Narrowing it here also makes the health endpoint testable without Postgres.
type healthChecker interface {
	Health(ctx context.Context) error
}

var _ healthChecker = (*database.Pool)(nil)

// Handlers are the feature handlers this server mounts.
//
// A nil handler means the feature is not wired yet, and its route is simply not
// registered — the server never invents a stub that returns a plausible-looking
// empty answer.
type Handlers struct {
	// Ingest serves POST /api/v1/admin/documents/ingest.
	Ingest http.Handler

	// QA serves POST /api/v1/qa/query as a Server-Sent Events stream.
	QA http.Handler

	// Citation serves GET /api/v1/citations/{id}.
	Citation http.Handler

	// Documents serves the catalogue under /api/v1/documents: the list, one
	// document with its versions, and a version's articles.
	Documents http.Handler

	// CorpusStats serves GET /api/v1/corpus/stats.
	CorpusStats http.Handler

	// Traces serves GET /api/v1/admin/traces/{request_id} — looks up what one
	// past request decided, for debugging.
	Traces http.Handler
}

// Server is the HTTP server and everything it needs.
type Server struct {
	cfg      config.Config
	log      *slog.Logger
	db       healthChecker
	handlers Handlers
	http     *http.Server
	limiter  *ipRateLimiter
}

// New builds the server and its routes.
func New(cfg config.Config, log *slog.Logger, db healthChecker, handlers Handlers) *Server {
	s := &Server{
		cfg:      cfg,
		log:      log,
		db:       db,
		handlers: handlers,
		limiter:  newIPRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst),
	}

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),

		// WriteTimeout is deliberately left at zero.
		//
		// It is an absolute deadline measured from the start of the request,
		// not an idle timeout, so any non-zero value silently truncates
		// long-lived SSE answer streams mid-sentence. Per-request deadlines
		// belong on the request context, where they can be set per route.
		WriteTimeout: 0,
	}

	return s
}

// Run serves until ctx is cancelled, then drains in-flight requests.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.log.Info("http server listening", slog.String("addr", s.cfg.Addr))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen: %w", err)
		}
	}()

	// Reclaim rate-limiter buckets for clients that have gone away.
	evictTicker := time.NewTicker(5 * time.Minute)
	defer evictTicker.Stop()
	go func() {
		for range evictTicker.C {
			s.limiter.evict(15 * time.Minute)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", slog.Duration("grace", s.cfg.ShutdownTimeout))

	// Detached from the cancelled ctx, or shutdown would abort immediately.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	s.log.Info("shutdown complete")
	return nil
}

// writeJSON writes v as a JSON response.
//
// The body is encoded before any header is written, so an encoding failure
// still produces a clean 500 rather than a 200 with a truncated body.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.ErrorContext(r.Context(), "encode response",
			slog.String("path", r.URL.Path),
			slog.Any("error", err))
		http.Error(w, `{"error":{"code":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// errorResponse is the single error shape every endpoint returns, so the
// frontend has one thing to parse.
type errorResponse struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

// writeError writes a client-safe error. The message is shown to users, so it
// never carries internal detail — that goes to the log, keyed by request ID.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	var resp errorResponse
	resp.Error.Code = code
	resp.Error.Message = message
	resp.Error.RequestID = middleware.GetReqID(r.Context())
	writeJSON(w, r, status, resp)
}
