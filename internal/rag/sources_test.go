package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/citations"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

type stubResolver struct {
	byID map[legal.ChunkID]citations.Citation
	err  error
	got  []legal.ChunkID
}

func (s *stubResolver) ResolveSome(_ context.Context, ids []legal.ChunkID) ([]citations.Citation, error) {
	s.got = ids
	if s.err != nil {
		return nil, s.err
	}
	out := make([]citations.Citation, 0, len(ids))
	for _, id := range ids {
		if c, ok := s.byID[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func carriedCitation(id, article string) citations.Citation {
	return citations.Citation{
		ChunkID:       legal.ChunkID(id),
		DocumentTitle: "Commercial Code",
		ArticleNo:     article,
		Language:      legal.LanguageEnglish,
		Text:          "The text of article " + article + ", as stored.",
	}
}

// The point of carrying sources: a follow-up about a provision already on screen
// does not depend on retrieval finding it again.
func TestCarriedSourcesLeadTheContext(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "265"),
	}}
	llm := &stubLLM{chunks: []string{"An answer [S1]."}}

	svc := NewService(
		&stubRetriever{results: testResults(2)},
		PassthroughReranker{}, llm, DefaultConfig(),
		testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "explain that one",
		CarriedChunks: []legal.ChunkID{"carried-1"},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	sources := events[0].Citations
	if len(sources) == 0 {
		t.Fatal("no sources emitted")
	}
	if sources[0].ChunkID != "carried-1" {
		t.Errorf("first source is %q, want the carried one", sources[0].ChunkID)
	}
	// The text comes from the database, not from anything a client sent.
	if !strings.Contains(llm.got.Messages[0].Content, "as stored") {
		t.Error("the carried provision's stored text did not reach the generator")
	}
}

func TestCarriedSourcesAreNotDuplicatedByRetrieval(t *testing.T) {
	// Retrieval finds the same chunk the conversation was already carrying.
	retrieved := testResults(1)
	retrieved[0].ChunkID = "carried-1"

	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "265"),
	}}

	svc := NewService(&stubRetriever{results: retrieved},
		PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "explain that one",
		CarriedChunks: []legal.ChunkID{"carried-1"},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if got := len(events[0].Citations); got != 1 {
		t.Errorf("emitted %d sources for one chunk, want 1", got)
	}
}

func TestAnswerSurvivesAFailedCarry(t *testing.T) {
	resolver := &stubResolver{err: errors.New("database busy")}

	svc := NewService(&stubRetriever{results: testResults(2)},
		PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "explain that one",
		CarriedChunks: []legal.ChunkID{"carried-1"},
	})
	if err != nil {
		t.Fatalf("a failed carry must not fail the answer: %v", err)
	}
	if len(events[0].Citations) == 0 {
		t.Error("retrieval results were lost along with the carried sources")
	}
}

// A conversation about provisions the corpus can no longer find is still a
// conversation: carried sources alone are enough to answer from.
func TestCarriedSourcesPreventAnUnnecessaryAbstention(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "265"),
	}}

	svc := NewService(&stubRetriever{results: nil}, // retrieval finds nothing
		PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "explain that one",
		CarriedChunks: []legal.ChunkID{"carried-1"},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Abstained {
		t.Error("abstained while holding the provision the question was about")
	}
}

// Regression test for a real failure found live, 2026-09-03: with a reranker
// threshold configured (the LLM reranker's 0.3, or Cohere's 0.15), every
// follow-up that carried sources abstained — including one whose answer,
// Article 499, this very conversation had already cited a turn earlier.
//
// mergeSources puts carried sources first, by design, and carriedResult leaves
// their Score at the zero value because no reranker scored them for this
// question. shouldAbstain then read top[0].Score and compared that zero against
// the threshold, so the comparison was true on every such request no matter how
// good retrieval had been. Every existing carried-source test missed it by
// using DefaultConfig()'s zero MinRerankScore, which never runs the check.
func TestCarriedSourcesSurviveARerankerThreshold(t *testing.T) {
	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "499"),
	}}

	cfg := DefaultConfig()
	cfg.MinRerankScore = 0.3 // matches rag.LLMRerankMinScore in shape, not value

	// Retrieval is good: testResults scores 0.9, comfortably over the bar.
	svc := NewService(&stubRetriever{results: testResults(2)},
		PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		cfg, testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "what are the conditions for a private limited company?",
		CarriedChunks: []legal.ChunkID{"carried-1"},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Abstained {
		t.Fatal("abstained on a carried source's unscored zero, discarding retrieval that cleared the threshold")
	}
	if len(done.Citations) != 1 || done.Citations[0].ChunkID != "carried-1" {
		t.Errorf("citations = %v, want the carried chunk", done.Citations)
	}
}

// When retrieval finds nothing above the bar, the provisions already on screen
// are the answer's evidence — and the only evidence. The rejected chunks are
// not kept as filler: something the reranker judged irrelevant is not improved
// by being cited.
func TestCarriedSourcesAnswerWhenRetrievalIsBelowThreshold(t *testing.T) {
	weak := testResults(2)
	for i := range weak {
		weak[i].Score = 0.02
	}

	resolver := &stubResolver{byID: map[legal.ChunkID]citations.Citation{
		"carried-1": carriedCitation("carried-1", "499"),
	}}

	cfg := DefaultConfig()
	cfg.MinRerankScore = 0.3

	svc := NewService(&stubRetriever{results: weak},
		PassthroughReranker{}, &stubLLM{chunks: []string{"An answer [S1]."}},
		cfg, testLogger(), WithSources(resolver))

	events, err := collect(t, svc, Question{
		Text:          "explain that one",
		CarriedChunks: []legal.ChunkID{"carried-1"},
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Abstained {
		t.Fatal("abstained while holding the provision the question was about")
	}

	sources := events[0].Citations
	if len(sources) != 1 || sources[0].ChunkID != "carried-1" {
		var ids []string
		for _, c := range sources {
			ids = append(ids, string(c.ChunkID))
		}
		t.Errorf("generator saw %v, want only the carried chunk", ids)
	}
}

func TestBestRerankedScoreIgnoresCarriedSources(t *testing.T) {
	carried := search.Result{ChunkID: "carried-1", Retriever: RetrieverCarried}

	t.Run("skips the leading carried zero", func(t *testing.T) {
		got, scored := bestRerankedScore([]search.Result{
			carried,
			{ChunkID: "chunk-1", Score: 0.8},
			{ChunkID: "chunk-2", Score: 0.4},
		})
		if !scored {
			t.Fatal("reported nothing scored while holding reranked results")
		}
		if got != 0.8 {
			t.Errorf("score = %v, want the best reranked score 0.8", got)
		}
	})

	t.Run("reports nothing scored when only carried remain", func(t *testing.T) {
		if _, scored := bestRerankedScore([]search.Result{carried}); scored {
			t.Error("treated a carried source's zero value as a reranker judgement")
		}
	})

	t.Run("reports nothing scored when empty", func(t *testing.T) {
		if _, scored := bestRerankedScore(nil); scored {
			t.Error("reported a score for no results")
		}
	})
}

func TestSearchTextOverridesTheQuestionForRetrieval(t *testing.T) {
	retriever := &stubRetriever{results: testResults(1)}

	svc := NewService(retriever, PassthroughReranker{},
		&stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger())

	if _, err := collect(t, svc, Question{
		Text:       "what about for a PLC?",
		SearchText: "What are the requirements for forming a private limited company?",
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if !strings.Contains(retriever.got.Text, "private limited company") {
		t.Errorf("retrieval searched for %q, want the rewrite", retriever.got.Text)
	}
}

func TestSuppliedCandidatesSkipRetrieval(t *testing.T) {
	retriever := &stubRetriever{results: testResults(3)}

	svc := NewService(retriever, PassthroughReranker{},
		&stubLLM{chunks: []string{"An answer [S1]."}},
		DefaultConfig(), testLogger())

	if _, err := collect(t, svc, Question{
		Text:       "How do I register a company?",
		Candidates: testResults(2),
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if retriever.got.Text != "" {
		t.Error("retrieved again despite being handed candidates")
	}
}

func TestDepthScalesHowMuchTheGeneratorSees(t *testing.T) {
	cfg := DefaultConfig()

	if got := cfg.topNFor(DepthQuote); got != 4 {
		t.Errorf("quote depth = %d, want 4", got)
	}
	if got := cfg.topNFor(DepthExplain); got != cfg.TopN {
		t.Errorf("explain depth = %d, want the default %d", got, cfg.TopN)
	}
	if got := cfg.topNFor(DepthAnalyse); got != maxAnalyseTopN {
		t.Errorf("analyse depth = %d, want %d", got, maxAnalyseTopN)
	}
	if got := cfg.topNFor(""); got != cfg.TopN {
		t.Errorf("unset depth = %d, want the default %d", got, cfg.TopN)
	}
}

func TestMergeSourcesKeepsCarriedFirstAndDropsDuplicates(t *testing.T) {
	carried := []search.Result{{ChunkID: "a"}, {ChunkID: "b"}}
	retrieved := []search.Result{{ChunkID: "b"}, {ChunkID: "c"}}

	got := mergeSources(carried, retrieved)

	var ids []string
	for _, r := range got {
		ids = append(ids, string(r.ChunkID))
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("order = %v, want carried first and one b", ids)
	}
}

func TestRelatedQuestionsComeFromTheCitations(t *testing.T) {
	related := relatedQuestions([]Citation{
		{DocumentTitle: "Commercial Code", ArticleNo: "265"},
		{DocumentTitle: "Labour Proclamation", ArticleNo: "12"},
	})

	if len(related) == 0 {
		t.Fatal("no follow-ups offered for a cited answer")
	}
	if len(related) > maxSuggestions {
		t.Errorf("offered %d follow-ups, want at most %d", len(related), maxSuggestions)
	}
	for _, s := range related {
		if !strings.Contains(s.Question, "Commercial Code") &&
			!strings.Contains(s.Question, "Labour Proclamation") {
			t.Errorf("follow-up mentions a document that was not cited: %q", s.Question)
		}
	}
}

func TestNoRelatedQuestionsWithoutCitations(t *testing.T) {
	if got := relatedQuestions(nil); got != nil {
		t.Errorf("offered %v for an answer with no sources", got)
	}
}
