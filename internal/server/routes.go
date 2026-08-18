package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// routes is the whole route table. Every endpoint the service exposes is
// visible here, in one screen.
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(recoverer(s.log))
	r.Use(requestLogger(s.log))
	r.Use(cors(s.cfg.CORSAllowedOrigins))

	// Operational endpoints: no rate limit, no CORS requirement, no versioning.
	r.Get("/healthz", s.handleHealth)

	r.Route("/api/v1", func(r chi.Router) {
		// Public API. Rate limited because every endpoint under it either costs
		// money per call or hits the database for an unauthenticated caller,
		// and body-capped at the small public limit.
		r.Group(func(r chi.Router) {
			r.Use(s.limiter.middleware)
			r.Use(limitBody(s.cfg.MaxRequestBytes))

			if s.handlers.QA != nil {
				r.Method(http.MethodPost, "/qa/query", s.handlers.QA)
			}
			if s.handlers.Citation != nil {
				// The endpoint that makes "click a citation, see the exact
				// source text" work.
				r.Method(http.MethodGet, "/citations/{id}", s.handlers.Citation)
			}
			if s.handlers.Documents != nil {
				// Browsing the corpus: read-only, published versions only.
				r.Mount("/documents", s.handlers.Documents)
			}
			if s.handlers.CorpusStats != nil {
				// What the corpus contains. Counted over the same published set
				// retrieval can see, so the figure on the home page is a claim
				// the system can stand behind.
				r.Method(http.MethodGet, "/corpus/stats", s.handlers.CorpusStats)
			}
		})

		// Admin API. Registered only when a token is configured: an
		// unauthenticated write path into the legal corpus must not exist even
		// by accident, so a missing token disables the route rather than
		// leaving it open.
		if s.handlers.Ingest != nil && !s.cfg.AdminToken.Empty() {
			r.Route("/admin", func(r chi.Router) {
				r.Use(requireAdminToken(s.cfg.AdminToken.Value()))
				// A parsed legal code is several megabytes of JSON; the public
				// limit would reject every real document.
				r.Use(limitBody(s.cfg.MaxIngestBytes))

				r.Method(http.MethodPost, "/documents/ingest", s.handlers.Ingest)
			})
		}
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "No such endpoint.")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
			"That method is not allowed on this endpoint.")
	})

	return r
}

// healthResponse is the shape the deployment probe and the developer both read.
type healthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// handleHealth reports whether the process can actually serve traffic, which
// means checking the database rather than just answering 200 because the
// goroutine scheduler is alive.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Health(r.Context()); err != nil {
		s.log.WarnContext(r.Context(), "health check failed",
			slog.String("component", "database"),
			slog.Any("error", err))
		writeJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status: "degraded",
			DB:     "down",
		})
		return
	}
	writeJSON(w, r, http.StatusOK, healthResponse{Status: "ok", DB: "up"})
}
