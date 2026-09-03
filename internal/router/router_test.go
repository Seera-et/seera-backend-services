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

// callScriptedLLM answers Complete differently depending on which system
// prompt it was asked with — translateSystemPrompt vs the router's own — so a
// test can assert what the classification call actually saw, not just what
// Route() ultimately returned.
type callScriptedLLM struct {
	translateResponse string
	translateErr      error
	classifyResponse  string

	classifyReq ai.Request
}

func (s *callScriptedLLM) Model() string { return "stub" }

func (s *callScriptedLLM) Stream(context.Context, ai.Request) (ai.Stream, error) {
	return nil, errors.New("router must not stream")
}

func (s *callScriptedLLM) Complete(_ context.Context, req ai.Request) (string, *ai.Usage, error) {
	if strings.Contains(req.System, "Translate the message to English") {
		if s.translateErr != nil {
			return "", nil, s.translateErr
		}
		return s.translateResponse, nil, nil
	}
	s.classifyReq = req
	return s.classifyResponse, nil, nil
}

func TestRouteTranslatesEthiopicScriptForClassification(t *testing.T) {
	llm := &callScriptedLLM{
		translateResponse: "What is needed to form a private limited company?",
		classifyResponse: decisionJSON(t, map[string]any{
			"intent": "legal", "depth": "explain",
			"search_text": "What is needed to form a private limited company?", "use_prior_sources": false,
		}),
	}

	got := newRouter(llm).Route(context.Background(), Input{
		Question: "የግል ውስን ኩባንያ ለማቋቋም ምን ያስፈልጋል?",
	})

	if got.Intent != IntentLegal {
		t.Errorf("intent = %q, want %q — found live 2026-08-28, an Amharic legal question misclassified as general/out_of_scope when classified on the untranslated text", got.Intent, IntentLegal)
	}
	if !strings.Contains(llm.classifyReq.Messages[0].Content, "private limited company") {
		t.Errorf("classification prompt = %q, want the English translation, not the Amharic original", llm.classifyReq.Messages[0].Content)
	}
}

func TestRouteClassifiesOriginalTextWhenTranslationFails(t *testing.T) {
	llm := &callScriptedLLM{
		translateErr: errors.New("provider down"),
		classifyResponse: decisionJSON(t, map[string]any{
			"intent": "legal", "depth": "explain",
			"search_text": "some rewrite", "use_prior_sources": false,
		}),
	}

	got := newRouter(llm).Route(context.Background(), Input{
		Question: "የግል ውስን ኩባንያ ለማቋቋም ምን ያስፈልጋል?",
	})

	if got.Fallback {
		t.Error("a translation failure should not fail the whole decision — classification should proceed on the original text")
	}
	if !strings.Contains(llm.classifyReq.Messages[0].Content, "የግል ውስን ኩባንያ") {
		t.Errorf("classification prompt = %q, want the original Amharic text when translation fails", llm.classifyReq.Messages[0].Content)
	}
}

func TestRouteSkipsTranslationForEnglishQuestions(t *testing.T) {
	llm := &callScriptedLLM{
		translateResponse: "should never be used",
		classifyResponse: decisionJSON(t, map[string]any{
			"intent": "legal", "depth": "explain",
			"search_text": "How do I register a company?", "use_prior_sources": false,
		}),
	}

	newRouter(llm).Route(context.Background(), Input{Question: "How do I register a company?"})

	if !strings.Contains(llm.classifyReq.Messages[0].Content, "How do I register a company?") {
		t.Errorf("classification prompt = %q, want the original English text untouched", llm.classifyReq.Messages[0].Content)
	}
}

// Found live, 2026-08-31: maxTurnRunes at 700 truncated a long structured
// answer before a lettered section ("B. Formation of a One Member PLC") the
// reader then asked about ("tell me more about... under B"), so the router
// never saw what "B" referred to. maxTurnRunes was raised to 4000; this locks
// in that a realistic long answer's later content now survives.
func TestBuildPromptKeepsLateContentWithinRaisedRuneCap(t *testing.T) {
	long := strings.Repeat("filler ", 500) + "B. Formation of a One Member PLC — MARKER"

	prompt := buildPrompt(Input{
		History: []Turn{{Role: ai.RoleAssistant, Text: long}},
	})

	if !strings.Contains(prompt, "MARKER") {
		t.Error("a marker placed past the old 700-rune cap did not survive trimHistory")
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

func TestCategoryIsParsedForGeneralIntent(t *testing.T) {
	for _, category := range []Category{
		CategoryGreeting, CategoryGeneralKnowledge, CategoryProductQuestion,
		CategoryOutOfScope, CategoryClarificationNeeded,
	} {
		t.Run(string(category), func(t *testing.T) {
			llm := &stubLLM{response: decisionJSON(t, map[string]any{
				"intent": "general", "depth": "explain", "search_text": "",
				"use_prior_sources": false, "category": string(category),
			})}

			got := newRouter(llm).Route(context.Background(), Input{Question: "hi"})

			if got.Category != category {
				t.Errorf("category = %q, want %q", got.Category, category)
			}
		})
	}
}

func TestUnknownCategoryDegradesRatherThanFails(t *testing.T) {
	// A category only ever changes tone inside an already-guarded, non-legal
	// reply, so a value the model invented should not force the whole
	// decision into the legal fallback.
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "general", "depth": "explain", "search_text": "",
		"use_prior_sources": false, "category": "philosophical_musing",
	})}

	got := newRouter(llm).Route(context.Background(), Input{Question: "hi"})

	if got.Fallback {
		t.Error("an unrecognized category should not trigger the legal fallback")
	}
	if got.Intent != IntentGeneral {
		t.Errorf("intent = %q, want %q", got.Intent, IntentGeneral)
	}
	if got.Category != "" {
		t.Errorf("category = %q, want empty for an unrecognized value", got.Category)
	}
}

func TestCategoryStaysEmptyForLegalAndDiscussion(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
	}{
		{
			name: "legal",
			fields: map[string]any{
				"intent": "legal", "depth": "explain",
				"search_text": "What does Article 5 say?", "use_prior_sources": false,
				"category": "general_knowledge",
			},
		},
		{
			name: "discussion",
			fields: map[string]any{
				"intent": "discussion", "depth": "explain",
				"search_text": "What does Article 5 say?", "use_prior_sources": true,
				"category": "greeting",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &stubLLM{response: decisionJSON(t, tt.fields)}
			got := newRouter(llm).Route(context.Background(), Input{
				Question:     "What does Article 5 say?",
				PriorSources: []string{"Commercial Code, Article 265"},
			})

			if got.Category != "" {
				t.Errorf("category = %q, want empty even though the model set one", got.Category)
			}
		})
	}
}

func TestSourcesSufficientRequiresPriorSources(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "discussion", "depth": "explain",
		"search_text": "What are the advantages of a PLC?", "use_prior_sources": true,
		"sources_sufficient": true,
	})}

	// No PriorSources on the Input — the model claiming sources_sufficient
	// cannot be trusted about sources the router itself has no record of.
	got := newRouter(llm).Route(context.Background(), Input{Question: "what about its advantages?"})

	if got.SourcesSufficient {
		t.Error("sources_sufficient was honored with no prior sources present")
	}
}

func TestSourcesSufficientOnlySettableForDiscussion(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "legal", "depth": "explain",
		"search_text": "What does Article 5 say?", "use_prior_sources": false,
		"sources_sufficient": true,
	})}

	got := newRouter(llm).Route(context.Background(), Input{
		Question:     "What does Article 5 say?",
		PriorSources: []string{"Commercial Code, Article 265"},
	})

	if got.Intent != IntentLegal {
		t.Fatalf("intent = %q, want %q", got.Intent, IntentLegal)
	}
	if got.SourcesSufficient {
		t.Error("sources_sufficient was honored for a non-discussion intent")
	}
}

func TestSourcesSufficientIsHonoredForDiscussionWithPriorSources(t *testing.T) {
	llm := &stubLLM{response: decisionJSON(t, map[string]any{
		"intent": "discussion", "depth": "explain",
		"search_text": "What are the advantages of a PLC?", "use_prior_sources": true,
		"sources_sufficient": true,
	})}

	got := newRouter(llm).Route(context.Background(), Input{
		Question:     "what about its advantages?",
		PriorSources: []string{"Commercial Code, Article 265"},
	})

	if !got.SourcesSufficient {
		t.Error("sources_sufficient was not honored for discussion with prior sources present")
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
