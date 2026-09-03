package tracing

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Handler serves GET /api/v1/admin/traces/{request_id} — the read side of
// Tracer, for looking up what one past request decided. Admin-gated the same
// way ingestion is (see internal/server/routes.go); a single-record lookup,
// not a dashboard or a list endpoint — see Gap 6's plan note on why that's
// deliberately out of scope for this pass.
func (r *Repository) Handler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// "*" (chi's wildcard param), not a named segment: request_id contains
		// a literal slash (see internal/server/routes.go's comment on why the
		// route itself is a wildcard), and only the wildcard sees the whole
		// remainder of the path.
		id := strings.TrimSpace(chi.URLParam(req, "*"))
		if id == "" {
			writeErr(w, http.StatusBadRequest, "invalid_request", "A request_id is required.")
			return
		}

		rec, err := r.Get(req.Context(), id)
		switch {
		case err == nil:
		case errors.Is(err, ErrNotFound):
			writeErr(w, http.StatusNotFound, "trace_not_found", "No trace recorded for that request id.")
			return
		default:
			log.ErrorContext(req.Context(), "read request trace",
				slog.String("request_id", id), slog.Any("error", err))
			writeErr(w, http.StatusInternalServerError, "internal_error", "Could not load that trace.")
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(rec)
	}
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
