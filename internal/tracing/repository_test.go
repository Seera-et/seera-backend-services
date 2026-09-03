package tracing

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/dialogue"
	"github.com/Seera-et/seera-backend-services/internal/rag"
)

// Trace's early-return guards (empty RequestID, nil Result) must fire before
// any database call — proven here with a nil pool, since calling Exec on it
// would panic. Every other path needs a real Postgres connection, which this
// package has no fixture for, matching every other repository in this
// codebase (see internal/search/translation_test.go's comment on the same
// convention).
func TestTraceIgnoresIncompleteRecordsWithoutTouchingTheDatabase(t *testing.T) {
	r := NewRepository(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r.Trace(context.Background(), dialogue.Trace{RequestID: "", Result: &rag.Result{Kind: rag.KindLegal}})
	r.Trace(context.Background(), dialogue.Trace{RequestID: "req-1", Result: nil})
	// Reaching this line without a panic (a nil *pgxpool.Pool dereferenced by
	// Exec) is the assertion.
}
