package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Handler serves the account endpoints.
type Handler struct {
	accounts *Repository
	log      *slog.Logger
}

func NewHandler(accounts *Repository, log *slog.Logger) *Handler {
	return &Handler{accounts: accounts, log: log}
}

// Me serves GET /api/v1/me.
//
// It is also where an account first becomes a row: the frontend calls this once
// after sign-in, which is the natural moment to record the user without making
// every other authenticated request pay for a write. There is no sign-up
// endpoint because there is nothing to sign up — the token already proves
// Google vouched for this person.
func (h *Handler) Me() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFrom(r.Context())
		if !ok {
			// Unreachable behind requireUser. Guarded anyway so that mounting
			// this route without the middleware fails closed rather than
			// panicking on an empty user.
			h.fail(w, http.StatusUnauthorized, "unauthorized", "Sign in to use this endpoint.")
			return
		}

		account, err := h.accounts.Ensure(r.Context(), user)
		if err != nil {
			h.log.ErrorContext(r.Context(), "ensure account",
				slog.String("user_id", user.ID), slog.Any("error", err))
			h.fail(w, http.StatusInternalServerError, "internal_error",
				"Could not load your account.")
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// Identity is per-user and changes on sign-out; a shared cache must
		// never hand one person's account to the next request.
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(account)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
