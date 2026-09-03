package rag

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/citations"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// maxCarriedSources bounds how much of a conversation's history of citations can
// be dragged into one request. Enough for a reader to ask about any source on
// screen; not enough to fill the context with a long thread's worth.
const maxCarriedSources = 6

// SourceResolver re-reads chunks that were cited earlier in a conversation.
//
// The client sends identifiers and this reads the text, which is the whole
// point: a carried source is a database record, exactly like a freshly retrieved
// one. Nothing a client sends becomes part of an answer's evidence.
type SourceResolver interface {
	// ResolveSome returns the chunks that exist, skipping the ones that do not.
	//
	// Lenient on purpose, unlike the citation endpoint: a stale identifier from
	// an old browser tab should cost that one source, not the answer.
	ResolveSome(ctx context.Context, ids []legal.ChunkID) ([]citations.Citation, error)
}

// carriedSources resolves the chunks a follow-up is about.
//
// A failure here is not fatal. The worst case is an answer that re-retrieves
// what it could have carried, which is the behaviour of every request before
// this existed.
func (s *Service) carriedSources(ctx context.Context, ids []legal.ChunkID) []search.Result {
	if len(ids) == 0 || s.sources == nil {
		return nil
	}
	if len(ids) > maxCarriedSources {
		ids = ids[:maxCarriedSources]
	}

	start := time.Now()
	resolved, err := s.sources.ResolveSome(ctx, ids)
	if err != nil {
		s.log.WarnContext(ctx, "could not carry earlier sources",
			slog.Int("requested", len(ids)),
			slog.Any("error", err))
		return nil
	}

	out := make([]search.Result, 0, len(resolved))
	for _, c := range resolved {
		out = append(out, carriedResult(c))
	}

	s.log.DebugContext(ctx, "carried earlier sources",
		slog.Int("requested", len(ids)),
		slog.Int("resolved", len(out)),
		slog.Duration("elapsed", time.Since(start)))

	return out
}

// carriedResult converts a resolved citation into a retrieval result, so the
// context builder does not need to know where a source came from.
func carriedResult(c citations.Citation) search.Result {
	return search.Result{
		ChunkID:          c.ChunkID,
		ArticleID:        c.ArticleID,
		VersionID:        c.VersionID,
		DocumentID:       c.DocumentID,
		DocumentTitle:    c.DocumentTitle,
		IssuingAuthority: c.IssuingAuthority,
		ArticleNo:        c.ArticleNo,
		ArticleTitle:     c.ArticleTitle,
		SectionPath:      c.SectionPath,
		Language:         c.Language,
		EffectiveFrom:    c.EffectiveFrom,
		Text:             c.Text,
		Retriever:        RetrieverCarried,
	}
}

// RetrieverCarried marks a source that came from earlier in the conversation
// rather than from this request's retrieval. Visible in the logs and in the
// evaluation, where "the answer only used what it was already holding" is worth
// being able to see.
const RetrieverCarried search.Retriever = "carried"

// bestRerankedScore returns the highest score among results this request
// actually reranked, and whether there were any.
//
// Carried sources are skipped, and that is the whole point. Their Score is a
// zero value, not a judgement — no reranker saw them this time round — so any
// threshold that reads it concludes "irrelevant" about the one provision the
// reader is most plainly asking about. Because mergeSources puts carried
// sources first, reading top[0].Score did exactly that, and abstained on every
// follow-up that carried anything.
//
// Results arrive carried-block-first and then in reranked order, so the first
// non-carried entry is already the best scored one.
func bestRerankedScore(top []search.Result) (float64, bool) {
	for _, r := range top {
		if r.Retriever == RetrieverCarried {
			continue
		}
		return r.Score, true
	}
	return 0, false
}

// retrieve runs retrieval, or accepts what the caller already fetched.
func (s *Service) retrieve(
	ctx context.Context,
	q Question,
	searchText string,
) ([]search.Result, int64, error) {
	if q.Candidates != nil {
		// Speculatively retrieved while the router was deciding.
		return q.Candidates, 0, nil
	}

	start := time.Now()
	candidates, err := s.retriever.Search(ctx, search.Query{
		Text:     searchText,
		Language: q.Language,
		AsOf:     q.AsOf,
		Limit:    s.cfg.Candidates,
	})
	if err != nil {
		if rateLimited(err) {
			return nil, 0, fmt.Errorf("retrieve: %w: %w", ErrProviderQuota, err)
		}
		return nil, 0, fmt.Errorf("retrieve: %w", err)
	}
	return candidates, time.Since(start).Milliseconds(), nil
}

// rerankTop reduces the candidate set to the few that reach the generator,
// and reports whether the reranker actually produced the ordering.
//
// The second return matters beyond logging: shouldAbstain's threshold is
// calibrated to a specific reranker's score scale (0.15 for Cohere's
// relevance score, LLMRerankMinScore for the LLM reranker's 0-1 judgment).
// A failed rerank falls back to raw fusion order, whose RRF scores live on a
// completely different, much smaller scale — comparing them against a
// reranker's threshold would abstain on nearly everything, not degrade
// gracefully. The caller uses this to skip that comparison when reranking
// didn't happen, exactly as done as if there were no reranker at all.
func (s *Service) rerankTop(
	ctx context.Context,
	searchText string,
	candidates []search.Result,
	topN int,
) ([]search.Result, bool) {
	if len(candidates) == 0 {
		return candidates, false
	}

	// Reranked against the same text retrieval used, not the raw follow-up:
	// scoring a pronoun against a provision is how a good candidate set gets
	// thrown away at the last step.
	top, err := s.reranker.Rerank(ctx, searchText, candidates, topN)
	if err != nil {
		// A reranker outage must not take the whole answer path down with it;
		// fused order is a worse ordering, not a wrong one.
		s.log.WarnContext(ctx, "reranker failed, falling back to fusion order",
			slog.String("reranker", s.reranker.Name()),
			slog.Any("error", err))
		if len(candidates) > topN {
			return candidates[:topN], false
		}
		return candidates, false
	}
	return top, true
}

// mergeSources puts carried sources first and drops duplicates.
//
// Order is the context budget's spending order, so what the reader is asking
// about is what survives if the budget runs out.
func mergeSources(carried, retrieved []search.Result) []search.Result {
	if len(carried) == 0 {
		return retrieved
	}

	seen := make(map[legal.ChunkID]bool, len(carried)+len(retrieved))
	out := make([]search.Result, 0, len(carried)+len(retrieved))

	for _, group := range [][]search.Result{carried, retrieved} {
		for _, r := range group {
			if seen[r.ChunkID] {
				continue
			}
			seen[r.ChunkID] = true
			out = append(out, r)
		}
	}
	return out
}
