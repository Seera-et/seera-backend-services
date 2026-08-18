package documents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Catalogue is the read API this handler exposes.
type Catalogue interface {
	List(ctx context.Context, f ListFilter) (ListResult, error)
	Get(ctx context.Context, id legal.DocumentID) (Detail, error)
	Articles(ctx context.Context, documentID legal.DocumentID, f ArticleFilter) (ArticlePage, error)
	Stats(ctx context.Context) (Stats, error)
}

// Handler serves the catalogue endpoints.
type Handler struct {
	catalogue Catalogue
	log       *slog.Logger
}

func NewHandler(catalogue Catalogue, log *slog.Logger) *Handler {
	return &Handler{catalogue: catalogue, log: log}
}

// Routes mounts the document endpoints on their own subrouter.
//
//	GET /api/v1/documents
//	GET /api/v1/documents/{id}
//	GET /api/v1/documents/{id}/articles
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Get("/{id}/articles", h.articles)
	return r
}

// Stats serves GET /api/v1/corpus/stats.
func (h *Handler) Stats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := h.catalogue.Stats(r.Context())
		if err != nil {
			h.internal(w, r, "corpus stats", err, "Could not read the corpus statistics.")
			return
		}
		// Short cache: the number changes only when something is ingested, and
		// the home page asks for it on every visit.
		h.write(w, r, http.StatusOK, stats, "public, max-age=60")
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	filter, err := ParseListFilter(r.URL.Query())
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	result, err := h.catalogue.List(r.Context(), filter)
	if err != nil {
		h.internal(w, r, "list documents", err, "Could not load the document list.")
		return
	}
	h.write(w, r, http.StatusOK, result, "public, max-age=60")
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := h.documentID(w, r)
	if !ok {
		return
	}

	detail, err := h.catalogue.Get(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		h.notFound(w, r)
		return
	case isInvalidIdentifier(err):
		h.fail(w, r, http.StatusBadRequest, "invalid_request", "That is not a valid document id.")
		return
	default:
		h.internal(w, r, "get document", err, "Could not load that document.")
		return
	}

	h.write(w, r, http.StatusOK, detail, "public, max-age=60")
}

func (h *Handler) articles(w http.ResponseWriter, r *http.Request) {
	id, ok := h.documentID(w, r)
	if !ok {
		return
	}

	filter, err := ParseArticleFilter(r.URL.Query())
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	page, err := h.catalogue.Articles(r.Context(), id, filter)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		h.notFound(w, r)
		return
	case isInvalidIdentifier(err):
		h.fail(w, r, http.StatusBadRequest, "invalid_request",
			"That is not a valid document or version id.")
		return
	default:
		h.internal(w, r, "list articles", err, "Could not load that document's articles.")
		return
	}

	// A published version's text never changes — an amendment creates a new
	// version — so this is safe to cache for longer than the list.
	h.write(w, r, http.StatusOK, page, "public, max-age=3600")
}

func (h *Handler) documentID(w http.ResponseWriter, r *http.Request) (legal.DocumentID, bool) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		h.fail(w, r, http.StatusBadRequest, "invalid_request", "A document id is required.")
		return "", false
	}
	return legal.DocumentID(id), true
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, v any, cacheControl string) {
	body, err := json.Marshal(v)
	if err != nil {
		h.log.ErrorContext(r.Context(), "encode response", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal_error", "Could not encode the response.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, http.StatusNotFound, "document_not_found",
		"No published document with that id is in the corpus.")
}

// internal logs the cause and returns a safe message. The detail is keyed by
// request ID in the log; it never reaches the client.
//
// A caller that went away is not a fault. The browser cancels in-flight reads
// routinely — a filter changes, a component unmounts, a page navigates — and
// Postgres reports that as a cancelled context. Logging it at ERROR turns
// ordinary navigation into a stream of alarms and buries the failures that
// matter.
func (h *Handler) internal(w http.ResponseWriter, r *http.Request, what string, err error, message string) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		h.log.DebugContext(r.Context(), "client cancelled request",
			slog.String("path", r.URL.Path))
		// The status is written anyway: nobody is reading it, and the alternative
		// is a handler that returns without a response.
		h.fail(w, r, StatusClientClosedRequest, "client_cancelled",
			"The request was cancelled before it completed.")
		return
	}

	h.log.ErrorContext(r.Context(), what,
		slog.String("path", r.URL.Path),
		slog.Any("error", err))
	h.fail(w, r, http.StatusInternalServerError, "internal_error", message)
}

// StatusClientClosedRequest is nginx's 499. Not in net/http, and not a real IANA
// code, but it is what a cancelled request means and it keeps these out of the
// 5xx rate that an alert would watch.
const StatusClientClosedRequest = 499

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":       code,
			"message":    message,
			"request_id": middleware.GetReqID(r.Context()),
		},
	})
}

// isInvalidIdentifier reports whether the error is Postgres rejecting a
// malformed uuid, which is the client's mistake and not a server fault.
func isInvalidIdentifier(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid input syntax for type uuid") ||
		strings.Contains(msg, "invalid UUID")
}
