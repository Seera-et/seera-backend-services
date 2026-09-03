package dialogue

import (
	"context"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/Seera-et/seera-backend-services/internal/rag"
	"github.com/Seera-et/seera-backend-services/internal/router"
)

// Tracer records what a request decided, for debugging after the fact.
//
// Every real bug found in this codebase's live testing so far was diagnosed
// by grepping raw process logs while they were still on screen — nothing was
// queryable afterward. Tracer exists to answer "why did it answer this way"
// for one past request, not to replace logging or become a dashboard.
//
// Optional and best-effort by construction: WithTracer is the only way to
// set one, Answer defers the call unconditionally so it runs whether the
// answer succeeded, failed, or the client disconnected, and Trace itself
// takes no error return — a tracer must never be able to fail the answer it
// is only supposed to be describing.
type Tracer interface {
	Trace(ctx context.Context, t Trace)
}

// Trace is one request's decision shape — never its content. No question
// text and no answer text: this is a debugging record of what the pipeline
// decided (which path, how much evidence, how it turned out), not a
// transcript store, so it carries none of the retention or privacy weight a
// content log would.
type Trace struct {
	RequestID string

	RouteIntent   string
	RouteCategory string
	RouteDepth    string
	RouteFallback bool

	// Result is the completed answer's summary, exactly what EventDone
	// carried to the client. Never mutated before being recorded — a trace
	// describes what the reader was actually shown.
	Result *rag.Result
}

// capturingEmit wraps emit so the caller can read the final Result once the
// answer finishes, without changing what the real caller receives — every
// event still passes through unchanged, in the same order.
func capturingEmit(emit func(rag.Event) error, captured **rag.Result) func(rag.Event) error {
	return func(e rag.Event) error {
		if e.Type == rag.EventDone {
			*captured = e.Result
		}
		return emit(e)
	}
}

// recordTrace is the deferred call in Answer. A nil tracer or a nil result
// (the request errored, or the client disconnected before EventDone) means
// there is nothing to record, not an error to handle.
func (s *Service) recordTrace(ctx context.Context, decision router.Decision, result *rag.Result) {
	if s.tracer == nil || result == nil {
		return
	}
	s.tracer.Trace(ctx, Trace{
		RequestID:     middleware.GetReqID(ctx),
		RouteIntent:   string(decision.Intent),
		RouteCategory: string(decision.Category),
		RouteDepth:    string(decision.Depth),
		RouteFallback: decision.Fallback,
		Result:        result,
	})
}
