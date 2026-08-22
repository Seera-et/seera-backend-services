package qa

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/Seera-et/seera-backend-services/internal/rag"
)

// Answerer is the answer path this handler exposes.
type Answerer interface {
	Answer(ctx context.Context, q rag.Question, emit func(rag.Event) error) error
}

// Handler serves POST /api/v1/qa/query as a stream.
type Handler struct {
	answerer Answerer
	log      *slog.Logger
}

func NewHandler(answerer Answerer, log *slog.Logger) *Handler {
	return &Handler{answerer: answerer, log: log}
}

// ServeHTTP answers one question.
//
// The response is Server-Sent Events, not JSON, which shapes the error handling:
// once the stream has started the status code is already 200 and cannot be taken
// back, so failures after that point are delivered as an `error` event instead.
// Everything that can be validated is therefore validated before the first byte.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := middleware.GetReqID(r.Context())

	var req Request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.failBeforeStream(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	question, err := req.Validate()
	if err != nil {
		h.failBeforeStream(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	stream, err := newSSEWriter(w)
	if err != nil {
		h.log.ErrorContext(r.Context(), "cannot stream", slog.Any("error", err))
		h.failBeforeStream(w, r, http.StatusInternalServerError, "internal_error",
			"Streaming is not available.")
		return
	}

	emit := func(event rag.Event) error {
		switch event.Type {
		case rag.EventRoute:
			if event.Route == nil {
				return nil
			}
			return stream.send("route", routePayload{
				Intent:         event.Route.Intent,
				Depth:          event.Route.Depth,
				SearchText:     event.Route.SearchText,
				CarriedSources: event.Route.CarriedSources,
			})
		case rag.EventSources:
			return stream.send("sources", sourcesPayload{Citations: event.Citations})
		case rag.EventToken:
			return stream.send("token", tokenPayload{Text: event.Text})
		case rag.EventDone:
			payload := donePayload{
				Citations: event.Citations,
				RequestID: requestID,
			}
			payload.Kind = string(rag.KindLegal)
			if event.Result != nil {
				if event.Result.Kind != "" {
					payload.Kind = string(event.Result.Kind)
				}
				payload.Abstained = event.Result.Abstained
				payload.Grounded = event.Result.Grounded
				payload.Timings = event.Result.Timings
				payload.Model = event.Result.Model
				payload.Reranker = event.Result.Reranker
				payload.Truncated = event.Result.Truncated
				payload.Intent = event.Result.Intent
				payload.Depth = event.Result.Depth
				payload.Related = event.Result.Related
			}
			if payload.Citations == nil {
				payload.Citations = []rag.Citation{}
			}
			return stream.send("done", payload)
		default:
			return nil
		}
	}

	if err := h.answerer.Answer(r.Context(), question, emit); err != nil {
		// A disconnected client is not an error worth reporting to anyone: the
		// answer stopped because nobody was listening, which is the intended
		// behaviour.
		if errors.Is(err, ErrClientGone) || errors.Is(r.Context().Err(), context.Canceled) {
			h.log.InfoContext(r.Context(), "client disconnected mid-answer")
			return
		}

		code, message := "answer_failed", "Something went wrong while answering. Please try again."
		level := slog.LevelError
		if errors.Is(err, rag.ErrProviderQuota) {
			// Not a fault in this service, and not worth a retry right now.
			code = "quota_exhausted"
			message = "The service is over its AI provider quota at the moment. " +
				"Please try again later."
			level = slog.LevelWarn
		}

		h.log.Log(r.Context(), level, "answer failed",
			slog.String("request_id", requestID),
			slog.String("code", code),
			slog.Any("error", err))

		// Best effort: the stream may already be broken, and there is nothing
		// further to do if this fails.
		_ = stream.send("error", errorPayload{
			Code:      code,
			Message:   message,
			RequestID: requestID,
		})
	}
}

// failBeforeStream writes an ordinary JSON error, for failures that happen while
// the status code is still ours to choose.
func (h *Handler) failBeforeStream(w http.ResponseWriter, r *http.Request, status int, code, message string) {
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
