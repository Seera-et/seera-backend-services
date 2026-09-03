package rag

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// The second retrieval pass.
//
// One retrieval answers "what is relevant to this question". It cannot answer
// "what else does the answer to this question depend on" — a provision that
// defines a term used by the first one, an exception two articles later, the
// article a cross-reference points at. Those are found by reading the first
// results, which is something only the model can do.
//
// So: retrieve, show the model what came back, ask what is missing, retrieve
// that, then answer once with both sets. It costs a model call and a retrieval,
// and it is off unless Config.MultiHop says otherwise.
const (
	// expandTimeout bounds the gap check. It sits between retrieval and the
	// first token, so it is spending the latency budget directly.
	expandTimeout = 6 * time.Second

	// expandMaxTokens caps the plan. It is a short list.
	expandMaxTokens = 200

	// maxExtraArticles and maxExtraResults bound what one expansion can add, so
	// a model that asks for everything gets a few things.
	maxExtraArticles = 4
	maxExtraResults  = 6
)

// ArticleFinder looks provisions up by number, which is how the second pass
// fetches the specific articles the first answer turned out to need.
type ArticleFinder interface {
	ByArticles(ctx context.Context, q search.Query, numbers []string) ([]search.Result, error)
}

const expandPrompt = `You are helping a legal research system decide whether it has everything it needs.

You are given a question and the provisions retrieved for it. Identify what is
missing — a definition a retrieved provision relies on, an exception or condition
it cross-references, or a closely related rule that the question cannot be
answered correctly without.

Rules:
- Only name an article number if one of the provisions below actually refers to
  it. Do not guess numbers.
- "follow_up_query" is a short search phrase for a gap you cannot name by number.
  Leave it empty if there is none.
- If the provisions are sufficient, return empty lists. That is the common case
  and a perfectly good answer.
- Never write legal text, and never answer the question.`

var expandSchema = &ai.Schema{
	Type: "object",
	Properties: map[string]*ai.Schema{
		"article_numbers": {
			Type:        "array",
			Items:       &ai.Schema{Type: "string"},
			Description: "Article numbers referred to by the provisions above and not among them.",
		},
		"follow_up_query": {
			Type:        "string",
			Description: "A short search phrase for a remaining gap, or empty.",
		},
	},
	Required: []string{"article_numbers", "follow_up_query"},
}

// expand runs the second pass and returns the enlarged result set.
//
// Every failure returns the original set unchanged: an answer that could have
// been better is not a reason to have no answer. reranked mirrors rerankTop's
// second return on every path — unchanged from the value passed in for the
// early-exit cases, and the fresh rerank's own status on the final path,
// where a second rerank actually happens.
func (s *Service) expand(
	ctx context.Context,
	q Question,
	searchText string,
	top []search.Result,
	reranked bool,
	topN int,
) ([]search.Result, bool, int64) {
	start := time.Now()

	expandCtx, cancel := context.WithTimeout(ctx, expandTimeout)
	defer cancel()

	plan, err := s.planExpansion(expandCtx, searchText, top)
	if err != nil {
		s.log.WarnContext(ctx, "second retrieval pass skipped",
			slog.Any("error", err))
		return top, reranked, time.Since(start).Milliseconds()
	}
	if len(plan.ArticleNumbers) == 0 && plan.FollowUpQuery == "" {
		// The common case: the first retrieval was enough.
		return top, reranked, time.Since(start).Milliseconds()
	}

	extra := s.fetchExpansion(expandCtx, q, plan)
	if len(extra) == 0 {
		return top, reranked, time.Since(start).Milliseconds()
	}

	merged := mergeSources(top, extra)
	s.log.DebugContext(ctx, "second retrieval pass added sources",
		slog.Int("articles_requested", len(plan.ArticleNumbers)),
		slog.Bool("follow_up_query", plan.FollowUpQuery != ""),
		slog.Int("added", len(merged)-len(top)))

	// Reranked again so the additions compete with the originals rather than
	// being appended to the end of a list the budget may never reach.
	rerankedTop, ok := s.rerankTop(ctx, searchText, merged, topN+maxExtraResults)
	return rerankedTop, ok, time.Since(start).Milliseconds()
}

type expansionPlan struct {
	ArticleNumbers []string `json:"article_numbers"`
	FollowUpQuery  string   `json:"follow_up_query"`
}

func (s *Service) planExpansion(
	ctx context.Context,
	searchText string,
	top []search.Result,
) (expansionPlan, error) {
	var b strings.Builder
	b.WriteString("Question: ")
	b.WriteString(searchText)
	b.WriteString("\n\nProvisions retrieved:\n\n")
	// The rendered sources block, reused so the gap check reads exactly what the
	// generator will read.
	b.WriteString(BuildContext(top, s.cfg.ContextTokenBudget).Block)

	raw, _, err := s.llm.Complete(ctx, ai.Request{
		System:     expandPrompt,
		Messages:   []ai.Message{{Role: ai.RoleUser, Content: b.String()}},
		MaxTokens:  expandMaxTokens,
		JSONSchema: expandSchema,
	})
	if err != nil {
		return expansionPlan{}, err
	}

	var plan expansionPlan
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &plan); err != nil {
		return expansionPlan{}, err
	}

	if len(plan.ArticleNumbers) > maxExtraArticles {
		plan.ArticleNumbers = plan.ArticleNumbers[:maxExtraArticles]
	}
	plan.FollowUpQuery = strings.TrimSpace(plan.FollowUpQuery)
	if len([]rune(plan.FollowUpQuery)) > 300 {
		plan.FollowUpQuery = ""
	}
	return plan, nil
}

// fetchExpansion retrieves what the plan asked for. Both arms are best-effort.
func (s *Service) fetchExpansion(
	ctx context.Context,
	q Question,
	plan expansionPlan,
) []search.Result {
	var extra []search.Result

	if len(plan.ArticleNumbers) > 0 && s.articles != nil {
		found, err := s.articles.ByArticles(ctx, search.Query{
			Language:     q.Language,
			AsOf:         q.AsOf,
			PerRetriever: maxExtraResults,
		}, plan.ArticleNumbers)
		if err != nil {
			s.log.WarnContext(ctx, "article lookup failed in second pass",
				slog.Any("error", err))
		} else {
			extra = append(extra, found...)
		}
	}

	if plan.FollowUpQuery != "" {
		found, err := s.retriever.Search(ctx, search.Query{
			Text:     plan.FollowUpQuery,
			Language: q.Language,
			AsOf:     q.AsOf,
			Limit:    maxExtraResults,
		})
		if err != nil {
			s.log.WarnContext(ctx, "follow-up query failed in second pass",
				slog.Any("error", err))
		} else {
			extra = append(extra, found...)
		}
	}

	if len(extra) > maxExtraResults {
		extra = extra[:maxExtraResults]
	}
	return extra
}
