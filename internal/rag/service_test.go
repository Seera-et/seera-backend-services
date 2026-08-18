package rag

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/ai"
	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

type stubRetriever struct {
	results []search.Result
	err     error
	got     search.Query
}

func (s *stubRetriever) Search(_ context.Context, q search.Query) ([]search.Result, error) {
	s.got = q
	return s.results, s.err
}

type stubLLM struct {
	chunks []string
	err    error
	got    ai.Request
}

func (s *stubLLM) Model() string { return "stub-model" }

func (s *stubLLM) Stream(_ context.Context, req ai.Request) (ai.Stream, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	return &stubStream{chunks: s.chunks}, nil
}

type stubStream struct {
	chunks []string
	i      int
	cur    ai.Event
}

func (s *stubStream) Next() bool {
	if s.i < len(s.chunks) {
		s.cur = ai.Event{Type: ai.EventText, Text: s.chunks[s.i]}
		s.i++
		return true
	}
	if s.i == len(s.chunks) {
		s.i++
		s.cur = ai.Event{Type: ai.EventDone}
		return true
	}
	return false
}

func (s *stubStream) Event() ai.Event { return s.cur }
func (s *stubStream) Err() error      { return nil }
func (s *stubStream) Close() error    { return nil }

type failingReranker struct{}

func (failingReranker) Name() string { return "failing" }
func (failingReranker) Rerank(context.Context, string, []search.Result, int) ([]search.Result, error) {
	return nil, errors.New("rerank service down")
}

func testResults(n int) []search.Result {
	out := make([]search.Result, n)
	for i := range out {
		out[i] = search.Result{
			ChunkID:       legal.ChunkID("chunk-" + string(rune('1'+i))),
			ArticleID:     legal.ArticleID("article-" + string(rune('1'+i))),
			DocumentTitle: "Commercial Code",
			ArticleNo:     string(rune('1' + i)),
			Language:      legal.LanguageEnglish,
			Text:          "A private limited company may be formed by two or more persons.",
			Score:         0.9,
		}
	}
	return out
}

func newTestService(r Retriever, llm ai.LLM, rr Reranker, cfg Config) *Service {
	return NewService(r, rr, llm, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func collect(t *testing.T, s *Service, q Question) ([]Event, error) {
	t.Helper()
	var events []Event
	err := s.Answer(context.Background(), q, func(e Event) error {
		events = append(events, e)
		return nil
	})
	return events, err
}

func TestAnswerEmitsSourcesBeforeTokens(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: testResults(2)},
		&stubLLM{chunks: []string{"A company ", "may be formed [S1]."}},
		PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "how do I form a company?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	if events[0].Type != EventSources {
		t.Fatalf("first event = %s, want sources — the UI renders citation cards "+
			"while the answer streams, which is most of the perceived-latency win", events[0].Type)
	}
	if len(events[0].Citations) != 2 {
		t.Errorf("sources event carried %d citations, want 2", len(events[0].Citations))
	}
	if events[len(events)-1].Type != EventDone {
		t.Errorf("last event = %s, want done", events[len(events)-1].Type)
	}
}

func TestAnswerReportsOnlyTheCitationsTheAnswerMade(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: testResults(3)},
		// Cites source 2 only, though three were retrieved.
		&stubLLM{chunks: []string{"The company must register [S2]."}},
		PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if len(done.Citations) != 1 || done.Citations[0].Marker != "S2" {
		t.Errorf("done carried %d citations (%v), want only the cited one — "+
			"returning all retrieved sources lets an ungrounded answer wear a full "+
			"set of plausible references", len(done.Citations), done.Citations)
	}
	if !done.Result.Grounded {
		t.Error("a cited answer should be grounded")
	}
}

func TestAnswerFlagsAnUngroundedAnswer(t *testing.T) {
	long := strings.Repeat("A company must register with the authority and pay the fee. ", 8)
	svc := newTestService(
		&stubRetriever{results: testResults(2)},
		&stubLLM{chunks: []string{long}}, // substantive, zero markers
		PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Grounded {
		t.Error("an answer making legal claims with no citations must be reported as ungrounded")
	}
	if len(done.Citations) != 0 {
		t.Errorf("ungrounded answer carried %d citations, want none", len(done.Citations))
	}
}

func TestAnswerAbstainsWithNoCandidates(t *testing.T) {
	llm := &stubLLM{chunks: []string{"should never be called"}}
	svc := newTestService(&stubRetriever{results: nil}, llm, PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "what is the law on space travel?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if !done.Result.Abstained {
		t.Error("no candidates must abstain rather than answer")
	}
	if llm.got.System != "" {
		t.Error("the model was called despite there being no evidence")
	}

	var text strings.Builder
	for _, e := range events {
		if e.Type == EventToken {
			text.WriteString(e.Text)
		}
	}
	if !strings.Contains(text.String(), "could not find") {
		t.Errorf("abstention message missing: %q", text.String())
	}
}

func TestAnswerAbstainsBelowTheRerankThreshold(t *testing.T) {
	weak := testResults(1)
	weak[0].Score = 0.02

	cfg := DefaultConfig()
	cfg.MinRerankScore = 0.15

	svc := newTestService(&stubRetriever{results: weak},
		&stubLLM{chunks: []string{"x"}}, PassthroughReranker{}, cfg)

	events, err := collect(t, svc, Question{Text: "unrelated question"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}
	if !events[len(events)-1].Result.Abstained {
		t.Error("a top score below the threshold must abstain")
	}
}

// A reranker outage should degrade the ordering, not take down the answer path:
// fused order is worse, not wrong.
func TestAnswerSurvivesRerankerFailure(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: testResults(3)},
		&stubLLM{chunks: []string{"Formed by two persons [S1]."}},
		failingReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "formation?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if done.Result.Abstained {
		t.Error("a reranker failure must not turn into an abstention")
	}
	if len(done.Citations) != 1 {
		t.Errorf("got %d citations, want 1", len(done.Citations))
	}
}

func TestAnswerPassesFiltersToRetrieval(t *testing.T) {
	retriever := &stubRetriever{results: testResults(1)}
	svc := newTestService(retriever, &stubLLM{chunks: []string{"x [S1]"}},
		PassthroughReranker{}, DefaultConfig())

	asOf := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := collect(t, svc, Question{
		Text: "what was the law", Language: legal.LanguageAmharic, AsOf: asOf,
	}); err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	if retriever.got.Language != legal.LanguageAmharic {
		t.Errorf("language filter = %q, want am", retriever.got.Language)
	}
	if !retriever.got.AsOf.Equal(asOf) {
		t.Errorf("as-of filter = %v, want %v — effectiveness is a SQL predicate, "+
			"never a model judgement", retriever.got.AsOf, asOf)
	}
}

func TestAnswerSendsSourcesAndQuestionToTheModel(t *testing.T) {
	llm := &stubLLM{chunks: []string{"x [S1]"}}
	svc := newTestService(&stubRetriever{results: testResults(1)}, llm,
		PassthroughReranker{}, DefaultConfig())

	if _, err := collect(t, svc, Question{Text: "how do I form a company?"}); err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	if llm.got.System != SystemPrompt {
		t.Error("the citation-required system prompt was not sent")
	}
	prompt := llm.got.Messages[0].Content
	if !strings.Contains(prompt, "[S1] Commercial Code") {
		t.Errorf("sources block missing provenance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "how do I form a company?") {
		t.Error("the question was not sent")
	}
	if strings.Index(prompt, "Sources:") > strings.Index(prompt, "Question:") {
		t.Error("sources must precede the question")
	}
}

// A disconnected client stops the answer instead of paying for tokens nobody
// will read.
func TestAnswerStopsWhenTheClientGoesAway(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: testResults(2)},
		&stubLLM{chunks: []string{"one ", "two ", "three "}},
		PassthroughReranker{}, DefaultConfig())

	gone := errors.New("client gone")
	var emitted int
	err := svc.Answer(context.Background(), Question{Text: "q"}, func(Event) error {
		emitted++
		if emitted == 2 {
			return gone
		}
		return nil
	})

	if !errors.Is(err, gone) {
		t.Errorf("Answer() = %v, want the emit error", err)
	}
	if emitted != 2 {
		t.Errorf("kept emitting after the client went away: %d events", emitted)
	}
}

func TestAnswerRejectsAnEmptyQuestion(t *testing.T) {
	svc := newTestService(&stubRetriever{}, &stubLLM{}, PassthroughReranker{}, DefaultConfig())

	if _, err := collect(t, svc, Question{Text: "   "}); err == nil {
		t.Error("Answer() accepted an empty question")
	}
}
