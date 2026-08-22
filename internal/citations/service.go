package citations

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Store is the persistence this service needs.
type Store interface {
	Get(ctx context.Context, id legal.ChunkID) (Citation, error)
	Resolve(ctx context.Context, ids []legal.ChunkID) ([]Citation, error)
	ResolveSome(ctx context.Context, ids []legal.ChunkID) ([]Citation, error)
}

// Service resolves citations and serves them.
type Service struct {
	store Store
	log   *slog.Logger
}

func NewService(store Store, log *slog.Logger) *Service {
	return &Service{store: store, log: log}
}

// Resolve implements Resolver.
func (s *Service) Resolve(ctx context.Context, ids []legal.ChunkID) ([]Citation, error) {
	return s.store.Resolve(ctx, ids)
}

// ResolveSome returns the citations that exist and skips the rest. Used when an
// answer carries forward the sources a conversation already cited, where a stale
// identifier should cost one source rather than the answer.
func (s *Service) ResolveSome(ctx context.Context, ids []legal.ChunkID) ([]Citation, error) {
	return s.store.ResolveSome(ctx, ids)
}

// Handler serves GET /api/v1/citations/{id}.
//
// This is the endpoint that makes "click a citation and see the exact source
// text" work, and it is the only read endpoint the Q&A feature needs. It is
// deliberately not a document browse or search API — that is the Knowledge
// Explorer, a later feature.
func (s *Service) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if id == "" {
			s.fail(w, http.StatusBadRequest, "invalid_request", "A citation id is required.")
			return
		}

		citation, err := s.store.Get(r.Context(), legal.ChunkID(id))
		switch {
		case err == nil:
		case errors.Is(err, ErrNotFound):
			s.fail(w, http.StatusNotFound, "citation_not_found",
				"That citation does not resolve to a source in the corpus.")
			return
		default:
			// A malformed uuid reaches Postgres and comes back as a type error
			// rather than as no rows, so it lands here. It is still the client's
			// mistake, and it must not be reported as a server fault.
			if isInvalidIdentifier(err) {
				s.fail(w, http.StatusBadRequest, "invalid_request",
					"That is not a valid citation id.")
				return
			}
			s.log.ErrorContext(r.Context(), "resolve citation",
				slog.String("chunk_id", id), slog.Any("error", err))
			s.fail(w, http.StatusInternalServerError, "internal_error",
				"Could not load that citation.")
			return
		}

		body, err := json.Marshal(citation)
		if err != nil {
			s.log.ErrorContext(r.Context(), "encode citation", slog.Any("error", err))
			s.fail(w, http.StatusInternalServerError, "internal_error", "Could not load that citation.")
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// The corpus is versioned and append-only, so a resolved citation never
		// changes. Caching it is safe and takes the citation-lookup latency
		// target off the database entirely for repeat views.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(body)
	}
}

func isInvalidIdentifier(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid input syntax for type uuid") ||
		strings.Contains(msg, "invalid UUID")
}

func (s *Service) fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
