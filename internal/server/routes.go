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
		// Public API: browsing published law. No account needed — a visitor
		// should be able to read the corpus and judge whether it is worth
		// signing up for, and these endpoints cost a database read, not a
		// model call.
		//
		// optionalUser attaches the caller when they happen to be signed in,
		// purely so the rate limiter can charge them rather than their NAT.
		// An anonymous or stale-token request is served normally.
		r.Group(func(r chi.Router) {
			if s.handlers.Verifier != nil {
				r.Use(optionalUser(s.handlers.Verifier, s.log))
			}
			r.Use(s.limiter.middleware)
			r.Use(limitBody(s.cfg.MaxRequestBytes))

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

		// Authenticated API: everything that spends money or belongs to one
		// person. Registered only when tokens can actually be verified, for the
		// same reason the admin group is: a route that cannot identify its
		// caller must not exist rather than fall open.
		//
		// The frontend guards these routes too, but that guard is presentation.
		// This is the control — /qa/query costs a Gemini call per request and
		// is one curl away from anyone who opens the network tab.
		if s.handlers.Verifier != nil {
			r.Group(func(r chi.Router) {
				r.Use(requireUser(s.handlers.Verifier, s.log))
				r.Use(s.limiter.middleware)
				r.Use(limitBody(s.cfg.MaxRequestBytes))

				if s.handlers.Me != nil {
					r.Method(http.MethodGet, "/me", s.handlers.Me)
				}
				if s.handlers.QA != nil {
					r.Method(http.MethodPost, "/qa/query", s.handlers.QA)
				}
				if s.handlers.Business != nil {
					// The Business & License Advisor: deterministic, cited, and
					// registered only when a rule set can be loaded at all.
					r.Mount("/business", s.handlers.Business)
				}
			})
		}

		// Admin API. Registered only when a token is configured: an
		// unauthenticated write path into the legal corpus must not exist even
		// by accident, so a missing token disables the route rather than
		// leaving it open. Mounted whenever any admin handler is wired, not
		// just ingestion — trace lookup is read-only but still internal-only,
		// so it shares the same guard rather than getting a weaker one.
		hasAdminHandler := s.handlers.Ingest != nil || s.handlers.Traces != nil ||
			s.handlers.BusinessRules != nil
		if hasAdminHandler && !s.cfg.AdminToken.Empty() {
			r.Route("/admin", func(r chi.Router) {
				r.Use(requireAdminToken(s.cfg.AdminToken.Value()))

				if s.handlers.Ingest != nil {
					r.Group(func(r chi.Router) {
						// A parsed legal code is several megabytes of JSON; the
						// public limit would reject every real document.
						r.Use(limitBody(s.cfg.MaxIngestBytes))
						r.Method(http.MethodPost, "/documents/ingest", s.handlers.Ingest)
					})
				}
				if s.handlers.BusinessRules != nil {
					// A rule set is a few tens of kilobytes of curated JSON —
					// larger than the public limit, far smaller than a parsed
					// legal code, so it shares ingestion's generous cap rather
					// than getting a third limit of its own.
					r.Group(func(r chi.Router) {
						r.Use(limitBody(s.cfg.MaxIngestBytes))
						r.Method(http.MethodPost, "/business/rules", s.handlers.BusinessRules)
					})
				}
				if s.handlers.Traces != nil {
					// request_id is middleware.RequestID's own format
					// ("<host>/<short-id>-<counter>"), which contains a literal
					// slash. chi's radix tree tokenizes the path by "/" before a
					// param is ever matched against it — even a regex param like
					// {request_id:.*} only ever sees one segment — so a single
					// named param silently truncates at the slash. A trailing
					// wildcard is chi's own way to capture the rest of a path,
					// slashes included; read back via chi.URLParam(r, "*") in
					// tracing.Repository.Handler.
					r.Method(http.MethodGet, "/traces/*", s.handlers.Traces)
				}
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
