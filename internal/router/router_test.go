package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// stubLLM answers Complete from a script and records what it was asked.
type stubLLM struct {
	response string
	err      error
	got      ai.Request
}

func (s *stubLLM) Model() string { return "stub" }

func (s *stubLLM) Stream(context.Context, ai.Request) (ai.Stream, error) {
	return nil, errors.New("router must not stream")
}

func (s *stubLLM) Complete(_ context.Context, req ai.Request) (string, *ai.Usage, error) {
	s.got = req
	if s.err != nil {
		return "", nil, s.err
	}
	return s.response, nil, nil
}

func newRouter(llm ai.LLM) *Router {
	return New(llm, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func decisionJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(encoded)
}

func TestRouteReadsTheDecision(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent":            "legal",
		"depth":             "explain",
		"search_text":       "What are the requirements for forming a private limited company in Ethiopia?",
		"use_prior_sources": false,
		"reason":            "asks about company formation",
	})}

	got := newRouter(llm).Route(context.Background(), Input{
		Question: "How do I register a company?",
	})

	if got.Intent != IntentLegal {
		t.Errorf("intent = %q, want %q", got.Intent, IntentLegal)
	}
	if got.Depth != DepthExplain {
		t.Errorf("depth = %q, want %q", got.Depth, DepthExplain)
	}
	if !strings.Contains(got.SearchText, "private limited company") {
		t.Errorf("search text = %q, want the rewrite", got.SearchText)
	}
	if got.Fallback {
		t.Error("fallback set on a good decision")
	}
}

func TestRouteAsksForStructuredOutput(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "general", "depth": "explain", "search_text": "", "use_prior_sources": false,
	})}

	newRouter(llm).Route(context.Background(), Input{Question: "Hello"})

	if llm.got.JSONSchema == nil {
		t.Fatal("router did not constrain the response shape")
	}
	if llm.got.MaxTokens != MaxOutputTokens {
		t.Errorf("MaxTokens = %d, want %d", llm.got.MaxTokens, MaxOutputTokens)
	}
	// A decision is not an answer: the routing prompt must not invite one.
	if !strings.Contains(llm.got.System, "You do not answer the message") {
		t.Error("system prompt does not tell the model to route rather than answer")
	}
}

func TestGeneralIntentSearchesForNothing(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "general", "depth": "quote", "search_text": "", "use_prior_sources": false,
	})}

	got := newRouter(llm).Route(context.Background(), Input{Question: "Hi there"})

	if got.Intent != IntentGeneral {
		t.Fatalf("intent = %q, want %q", got.Intent, IntentGeneral)
	}
	if got.SearchText != "" {
		t.Errorf("search text = %q, want empty for a general reply", got.SearchText)
	}
}

// Every failure resolves to the corpus. This is the property that keeps a
// router outage from becoming a wrong answer.
func TestRouteFailsTowardsTheLaw(t *testing.T) {
	tests := []struct {
		name string
		llm  *stubLLM
	}{
		{"provider error", &stubLLM{err: errors.New("503")}},
		{"empty response", &stubLLM{response: "   "}},
		{"not json", &stubLLM{response: "I think this is a legal question."}},
		{"unknown intent", &stubLLM{response: `{"intent":"philosophy","depth":"explain"}`}},
		{"unknown depth", &stubLLM{response: `{"intent":"legal","depth":"ponder"}`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newRouter(tt.llm).Route(context.Background(), Input{
				Question: "What about for a PLC?",
				History: []Turn{
					{Role: ai.RoleUser, Text: "How do I register a company?"},
				},
			})

			if got.Intent != IntentLegal {
				t.Errorf("intent = %q, want %q on failure", got.Intent, IntentLegal)
			}
			if !got.Fallback {
				t.Error("fallback not recorded")
			}
			// Degraded, not broken: the previous question supplies the terms the
			// pronoun stands in for.
			if !strings.Contains(got.SearchText, "How do I register a company?") {
				t.Errorf("search text = %q, want the joined fallback query", got.SearchText)
			}
		})
	}
}

func TestDiscussionAlwaysCarriesItsSources(t *testing.T) {
	// The model said discussion but forgot the flag; the sources are the whole
	// point of the intent, so they are carried anyway.
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "discussion", "depth": "explain",
		"search_text":       "What does Article 265 of the Commercial Code require?",
		"use_prior_sources": false,
	})}

	got := newRouter(llm).Route(context.Background(), Input{
		Question:     "explain the second one",
		PriorSources: []string{"Commercial Code, Article 265"},
	})

	if got.Intent != IntentDiscussion {
		t.Fatalf("intent = %q, want %q", got.Intent, IntentDiscussion)
	}
	if !got.UsePriorSources {
		t.Error("a discussion must carry the sources being discussed")
	}
}

func TestPriorSourcesAreNotClaimedWhenThereAreNone(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "legal", "depth": "quote",
		"search_text": "What does Article 5 say?", "use_prior_sources": true,
	})}

	got := newRouter(llm).Route(context.Background(), Input{Question: "What does Article 5 say?"})

	if got.UsePriorSources {
		t.Error("claimed prior sources on the first turn of a conversation")
	}
}

func TestUnusableRewriteFallsBackButKeepsTheClassification(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "legal", "depth": "analyse",
		"search_text":       strings.Repeat("the model started answering instead ", 40),
		"use_prior_sources": false,
	})}

	got := newRouter(llm).Route(context.Background(), Input{Question: "Is that allowed?"})

	if got.Fallback {
		t.Error("a bad rewrite is not a failed classification")
	}
	if got.Depth != DepthAnalyse {
		t.Errorf("depth = %q, want the model's choice kept", got.Depth)
	}
	if got.SearchText != "Is that allowed?" {
		t.Errorf("search text = %q, want the original question", got.SearchText)
	}
}

func TestPromptCarriesTheConversationAndSources(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "discussion", "depth": "explain",
		"search_text": "x", "use_prior_sources": true,
	})}

	newRouter(llm).Route(context.Background(), Input{
		Question:     "explain the second one",
		History:      []Turn{{Role: ai.RoleUser, Text: "How do I register a company?"}},
		PriorSources: []string{"Commercial Code, Article 265", "Commercial Code, Article 499"},
	})

	prompt := llm.got.Messages[0].Content
	for _, want := range []string{
		"How do I register a company?",
		"Commercial Code, Article 499",
		"explain the second one",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestEmptyQuestionDoesNotCallTheModel(t *testing.T) {
	llm := &stubLLM{response: "{}"}
	got := newRouter(llm).Route(context.Background(), Input{Question: "   "})

	if llm.got.System != "" {
		t.Error("called the model for an empty message")
	}
	if got.Intent != IntentLegal || !got.Fallback {
		t.Errorf("got %+v, want the legal fallback", got)
	}
}
