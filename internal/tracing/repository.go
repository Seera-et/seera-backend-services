// Package tracing persists what a request decided — routing, retrieval
// outcome, timings — so a past answer can be inspected after the fact
// without grepping raw process logs while they are still on screen.
//
// It is a decision/shape record, not a transcript store: no question text
// and no answer text are ever written here. See dialogue.Trace's doc comment
// for the boundary this package is built to respect.
package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seera-et/seera-backend-services/internal/dialogue"
)

// ErrNotFound is returned when a request id has no trace recorded — either
// it never happened, tracing was disabled at the time, or the request never
// reached a completed answer (see dialogue.Tracer's doc comment: a nil
// Result is never written).
var ErrNotFound = errors.New("trace not found")

// writeTimeout bounds the insert. Short and deliberately decoupled from the
// request's own context (see Trace): a client that has already disconnected
// must not be able to race the write it triggered.
const writeTimeout = 2 * time.Second

// Repository is the Postgres-backed implementation of dialogue.Tracer, and
// serves the read side (Handler) that looks a trace back up by request id.
type Repository struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func NewRepository(pool *pgxpool.Pool, log *slog.Logger) *Repository {
	return &Repository{pool: pool, log: log}
}

// Trace implements dialogue.Tracer.
//
// Best-effort by construction, matching the interface's own contract: a
// write failure is logged and swallowed here, never returned to the caller —
// a tracer must not be able to affect the answer it is only supposed to be
// describing. Runs on its own short timeout against a context derived from
// context.Background(), not the request's own ctx: by the time this fires
// (a deferred call in dialogue.Service.Answer, after EventDone has already
// been emitted) the request may be moments from closing, and a canceled
// request context must not be able to kill a write the request itself
// triggered.
func (r *Repository) Trace(ctx context.Context, t dialogue.Trace) {
	if t.RequestID == "" || t.Result == nil {
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	timings, err := json.Marshal(t.Result.Timings)
	if err != nil {
		r.log.WarnContext(ctx, "encode trace timings", slog.Any("error", err))
		timings = []byte("{}")
	}

	confidence := ""
	if t.Result.Confidence != nil {
		confidence = string(t.Result.Confidence.Level)
	}

	const query = `
		INSERT INTO request_traces (
			request_id, route_intent, route_category, route_depth, route_fallback,
			kind, abstained, grounded, citation_count, confidence, model, reranker, timings
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (request_id) DO NOTHING`

	_, err = r.pool.Exec(writeCtx, query,
		t.RequestID, t.RouteIntent, t.RouteCategory, t.RouteDepth, t.RouteFallback,
		string(t.Result.Kind), t.Result.Abstained, t.Result.Grounded, len(t.Result.Citations),
		confidence, t.Result.Model, t.Result.Reranker, timings,
	)
	if err != nil {
		r.log.WarnContext(ctx, "write request trace",
			slog.String("request_id", t.RequestID), slog.Any("error", err))
	}
}

// Record is one trace, as read back.
type Record struct {
	RequestID     string          `json:"request_id"`
	CreatedAt     time.Time       `json:"created_at"`
	RouteIntent   string          `json:"route_intent"`
	RouteCategory string          `json:"route_category,omitempty"`
	RouteDepth    string          `json:"route_depth,omitempty"`
	RouteFallback bool            `json:"route_fallback"`
	Kind          string          `json:"kind"`
	Abstained     bool            `json:"abstained"`
	Grounded      bool            `json:"grounded"`
	CitationCount int             `json:"citation_count"`
	Confidence    string          `json:"confidence,omitempty"`
	Model         string          `json:"model,omitempty"`
	Reranker      string          `json:"reranker,omitempty"`
	Timings       json.RawMessage `json:"timings"`
}

// Get looks a trace up by request id.
func (r *Repository) Get(ctx context.Context, requestID string) (Record, error) {
	const query = `
		SELECT request_id, created_at, route_intent, route_category, route_depth,
		       route_fallback, kind, abstained, grounded, citation_count, confidence,
		       model, reranker, timings
		FROM request_traces WHERE request_id = $1`

	var rec Record
	err := r.pool.QueryRow(ctx, query, requestID).Scan(
		&rec.RequestID, &rec.CreatedAt, &rec.RouteIntent, &rec.RouteCategory, &rec.RouteDepth,
		&rec.RouteFallback, &rec.Kind, &rec.Abstained, &rec.Grounded, &rec.CitationCount,
		&rec.Confidence, &rec.Model, &rec.Reranker, &rec.Timings,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	return rec, nil
}
