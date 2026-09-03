package lang

import (
	"context"
	"errors"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/ai"
)

func TestHasEthiopicScript(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"amharic question", "የግል ውስን ኩባንያ ለማቋቋም ምን ያስፈልጋል?", true},
		{"ge'ez article reference", "አንቀጽ ፵፭ ምን ይላል?", true},
		{"plain english", "What is the minimum capital for a company?", false},
		{"english with an embedded amharic word", "What does ንግድ mean?", true},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasEthiopicScript(tc.text); got != tc.want {
				t.Errorf("HasEthiopicScript(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// stubLLM is a minimal ai.LLM for exercising Translate without a real provider.
type stubLLM struct {
	response string
	err      error
	got      ai.Request
}

func (s *stubLLM) Model() string { return "stub" }

func (s *stubLLM) Stream(context.Context, ai.Request) (ai.Stream, error) {
	return nil, errors.New("Translate must not stream")
}

func (s *stubLLM) Complete(_ context.Context, req ai.Request) (string, *ai.Usage, error) {
	s.got = req
	if s.err != nil {
		return "", nil, s.err
	}
	return s.response, nil, nil
}

func TestTranslate(t *testing.T) {
	llm := &stubLLM{response: "What is needed to form a private limited company?"}

	got, err := Translate(context.Background(), llm, "የግል ውስን ኩባንያ ለማቋቋም ምን ያስፈልጋል?")
	if err != nil {
		t.Fatalf("Translate() = %v", err)
	}
	if got != "What is needed to form a private limited company?" {
		t.Errorf("translation = %q", got)
	}
	if llm.got.MaxTokens != maxTokens {
		t.Errorf("MaxTokens = %d, want %d", llm.got.MaxTokens, maxTokens)
	}
}

func TestTranslateSurfacesProviderErrors(t *testing.T) {
	llm := &stubLLM{err: errors.New("provider down")}

	if _, err := Translate(context.Background(), llm, "ጥያቄ"); err == nil {
		t.Error("Translate() ignored a provider error")
	}
}
