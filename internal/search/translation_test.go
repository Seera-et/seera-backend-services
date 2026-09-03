package search

import (
	"context"
	"errors"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

// stubTranslateLLM is a minimal ai.LLM for exercising WithTranslation's wiring
// without a real provider. The translation behavior itself (script detection,
// the Translate call) is tested once in internal/lang, not duplicated here.
type stubTranslateLLM struct{}

func (s *stubTranslateLLM) Model() string { return "stub" }

func (s *stubTranslateLLM) Stream(context.Context, ai.Request) (ai.Stream, error) {
	return nil, errors.New("not implemented")
}

func (s *stubTranslateLLM) Complete(context.Context, ai.Request) (string, *ai.Usage, error) {
	return "", nil, nil
}

// TestSearchSkipsTranslationForEnglishQueries and the translation augmentation
// itself are not unit-tested at this level: Search hits a real database (this
// package has no DB fixture, matching every other Search-touching test here —
// see service_test.go's pure-function-only scope) and the augmentation's
// actual retrieval benefit is what tests/evaluation/dataset/edge_cases_v1.yaml's
// cross_language pattern measures against the real corpus instead.
func TestWithTranslationDefaultsToADiscardLoggerWhenNilIsPassed(t *testing.T) {
	s := &Service{}
	WithTranslation(&stubTranslateLLM{}, nil)(s)

	if s.log == nil {
		t.Error("WithTranslation(llm, nil) left log nil — Search would panic calling WarnContext on a translation failure")
	}
}
