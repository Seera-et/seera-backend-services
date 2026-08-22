package dialogue

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/rag"
	"github.com/Seera-et/seera-backend-services/internal/router"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// Speculative retrieval.
//
// The router sits in front of retrieval, so its latency is added to
// time-to-first-token, which has a 3-second budget. On the first turn of a
// conversation there is nothing to rewrite — the question is the search query —
// so retrieval can start immediately and run while the router decides.
//
// It is only started when it is likely to be used, and only used when it is
// certainly correct:
//
//   - Started when the conversation has no history. A follow-up has to wait for
//     the rewrite, because the rewrite is the entire reason a follow-up
//     retrieves anything useful.
//   - Used only when the router agrees the message is legal and did not change
//     the query. Any rewrite, however small, means the speculative results
//     answer a different question and are discarded.
//
// The cost of being wrong is one embedding and three index scans, paid on
// non-legal first messages. The cost of not doing it is the router's latency on
// every question.
type prefetch struct {
	done       chan struct{}
	candidates []search.Result
}

// startPrefetch begins retrieval if this turn qualifies. Returns nil when it
// does not, which the accessor handles.
func (s *Service) startPrefetch(ctx context.Context, q rag.Question) *prefetch {
	if s.prefetch == nil || len(q.History) > 0 {
		return nil
	}

	p := &prefetch{done: make(chan struct{})}

	go func() {
		defer close(p.done)

		results, err := s.prefetch.Search(ctx, search.Query{
			Text:     q.Text,
			Language: q.Language,
			AsOf:     q.AsOf,
			Limit:    s.candidates,
		})
		if err != nil {
			// Not reported as a failure: the legal engine will retrieve for
			// itself and surface the real error if there is one.
			s.log.DebugContext(ctx, "speculative retrieval failed, ignoring",
				slog.Any("error", err))
			return
		}
		p.candidates = results
	}()

	return p
}

// results returns the speculative candidates when they answer the question the
// router settled on, and nil otherwise — which tells the legal engine to
// retrieve for itself.
func (p *prefetch) results(question string, decision router.Decision) []search.Result {
	if p == nil {
		return nil
	}

	// Wait for the goroutine either way: abandoning it would leave retrieval
	// running against a context the caller is about to finish with.
	<-p.done

	if decision.Intent == router.IntentGeneral {
		return nil
	}
	if decision.UsePriorSources {
		// A discussion is answered from provisions already in hand; the
		// speculative set was fetched for the raw text and may be beside the
		// point. Let the legal engine retrieve against the rewrite.
		return nil
	}
	if strings.TrimSpace(decision.SearchText) != strings.TrimSpace(question) {
		return nil
	}
	if len(p.candidates) == 0 {
		// Empty could mean "nothing matched" or "the fetch failed". The legal
		// engine repeating the search settles it, and an abstention is too
		// important a conclusion to reach from a discarded goroutine.
		return nil
	}

	return p.candidates
}
