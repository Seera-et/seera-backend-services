package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Advisor is the API this handler exposes.
type Advisor interface {
	Form(ctx context.Context) (IntakeForm, error)
	Advise(ctx context.Context, intake Intake) (Recommendation, error)
}

// Publisher loads a rule set. Admin-only.
type Publisher interface {
	Publish(ctx context.Context, in RuleSetInput) (string, error)
}

// Handler serves the advisor endpoints.
type Handler struct {
	advisor Advisor
	log     *slog.Logger
}

func NewHandler(advisor Advisor, log *slog.Logger) *Handler {
	return &Handler{advisor: advisor, log: log}
}

// Routes mounts the public advisor endpoints.
//
//	GET  /api/v1/business/intake
//	POST /api/v1/business/advisor
//
// Plain JSON, not SSE. The whole recommendation is deterministic and comes back
// in a few milliseconds, so there is no first-token latency to hide and nothing
// to stream — the streaming contract exists for generated answers, and this
// path deliberately generates nothing. See Recommendation.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/intake", h.intake)
	r.Post("/advisor", h.advise)
	return r
}

func (h *Handler) intake(w http.ResponseWriter, r *http.Request) {
	form, err := h.advisor.Form(r.Context())
	switch {
	case err == nil:
	case errors.Is(err, ErrNoRuleSet):
		h.noRuleSet(w, r)
		return
	default:
		h.internal(w, r, "load intake form", err, "Could not load the advisor questions.")
		return
	}

	// The rule set changes only when an administrator publishes one, and the
	// questionnaire is fetched on every visit to the page.
	h.write(w, r, http.StatusOK, form, "public, max-age=60")
}

func (h *Handler) advise(w http.ResponseWriter, r *http.Request) {
	var req AdviseRequest
	if err := decodeJSON(r, &req); err != nil {
		h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	rec, err := h.advisor.Advise(r.Context(), req.Intake())
	switch {
	case err == nil:
	case errors.Is(err, ErrNoRuleSet):
		h.noRuleSet(w, r)
		return
	case errors.Is(err, ErrUnknownActivity):
		h.fail(w, r, http.StatusBadRequest, "invalid_request",
			"That is not an activity this advisor knows about. Fetch /business/intake for the current list.")
		return
	default:
		h.internal(w, r, "advise", err, "Could not produce a recommendation.")
		return
	}

	// Not cached: it is a POST, and the response is a function of the body.
	h.write(w, r, http.StatusOK, rec, "")
}

// PublishHandler serves POST /api/v1/admin/business/rules.
//
// Separate from Routes because it is mounted under the admin guard. Loading a
// rule set is writing legal content into the system, and it sits behind the same
// token as document ingestion for the same reason.
func PublishHandler(publisher Publisher, log *slog.Logger) http.Handler {
	h := &Handler{log: log}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in RuleSetInput
		if err := decodeJSON(r, &in); err != nil {
			h.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		id, err := publisher.Publish(r.Context(), in)
		switch {
		case err == nil:
		case errors.Is(err, ErrUnknownArticle):
			// The author cited law the corpus does not have. Their mistake to
			// fix, and the message has to name it precisely enough to fix.
			h.fail(w, r, http.StatusUnprocessableEntity, "unverifiable_citation", err.Error())
			return
		default:
			// Validation errors are the common case here and are safe to return
			// verbatim: this endpoint's caller is an administrator holding the
			// token, and a vague message would make a malformed rule set
			// impossible to debug.
			log.ErrorContext(r.Context(), "publish rule set", slog.Any("error", err))
			h.fail(w, r, http.StatusBadRequest, "invalid_rule_set", err.Error())
			return
		}

		log.InfoContext(r.Context(), "business rule set published",
			slog.String("rule_set_id", id),
			slog.String("label", in.Label),
			slog.Int("structures", len(in.Structures)),
			slog.Int("activities", len(in.Activities)),
			slog.Int("requirements", len(in.Requirements)))

		h.write(w, r, http.StatusOK, map[string]any{
			"rule_set_id":  id,
			"label":        in.Label,
			"structures":   len(in.Structures),
			"activities":   len(in.Activities),
			"requirements": len(in.Requirements),
		}, "")
	})
}

// maxAdviseBody caps the intake body. The request is a handful of scalars; the
// server-wide limit is generous by comparison and there is no reason to accept
// a megabyte of JSON here.
const maxAdviseBody = 64 << 10

func decodeJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxAdviseBody))
	// Unknown fields are rejected so a client sending "capital" instead of
	// "capital_birr" hears about it, rather than getting a recommendation
	// computed without the number they supplied.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(into); err != nil {
		return errors.New("the request body is not the JSON this endpoint expects: " + err.Error())
	}
	return nil
}

func (h *Handler) noRuleSet(w http.ResponseWriter, r *http.Request) {
	// 503, not 404: the endpoint exists and will work once an administrator
	// publishes a rule set. A 404 would tell a client the feature is absent.
	h.fail(w, r, http.StatusServiceUnavailable, "no_rule_set",
		"The advisor has no published rule set on this deployment, so it cannot recommend a business structure yet.")
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

// internal logs the cause and returns a safe message, treating a cancelled
// request as ordinary rather than as a fault — same reasoning as
// documents.Handler.internal.
func (h *Handler) internal(w http.ResponseWriter, r *http.Request, what string, err error, message string) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		h.log.DebugContext(r.Context(), "client cancelled request",
			slog.String("path", r.URL.Path))
		h.fail(w, r, 499, "client_cancelled", "The request was cancelled before it completed.")
		return
	}

	h.log.ErrorContext(r.Context(), what,
		slog.String("path", r.URL.Path),
		slog.Any("error", err))
	h.fail(w, r, http.StatusInternalServerError, "internal_error", message)
}

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
