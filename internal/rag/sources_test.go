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
