package chat

import (
	"context"
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
	calls    int
}

func (s *stubLLM) Model() string { return "stub" }

func (s *stubLLM) Stream(context.Context, ai.Request) (ai.Stream, error) {
	return nil, errors.New("chat must not stream")
}

func (s *stubLLM) Complete(_ context.Context, req ai.Request) (string, *ai.Usage, error) {
	s.calls++
	s.got = req
	if s.err != nil {
		return "", nil, s.err
	}
	return s.response, nil, nil
}

func newEngine(llm ai.LLM) *Engine {
	return New(llm, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestOutOfScopeNeverCallsTheModel(t *testing.T) {
	llm := &stubLLM{response: "should never be used"}

	reply, err := newEngine(llm).Reply(context.Background(), Input{
		Message:  "Who won the world cup?",
		Category: CategoryOutOfScope,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if llm.calls != 0 {
		t.Error("out-of-scope reply called the model")
	}
	if reply.Text != OutOfScopeMessage {
		t.Errorf("reply = %q, want the canned out-of-scope message", reply.Text)
	}
	if reply.Deferred {
		t.Error("the canned out-of-scope reply must not be deferred")
	}
}

func TestEachCategoryCarriesItsGuidanceInTheSystemPrompt(t *testing.T) {
	tests := []struct {
		category Category
		want     string
	}{
		{CategoryGreeting, "greeting, thanks, or pure pleasantry"},
		{CategoryGeneralKnowledge, "asks for something"},
		{CategoryProductQuestion, "what Seera is, what it can do"},
		{CategoryClarificationNeeded, "exactly one short clarifying question"},
	}

	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			llm := &stubLLM{response: "a reply with nothing to trip the guard"}

			if _, err := newEngine(llm).Reply(context.Background(), Input{
				Message: "hi", Category: tt.category,
			}); err != nil {
				t.Fatalf("Reply: %v", err)
			}

			if !strings.Contains(llm.got.System, tt.want) {
				t.Errorf("system prompt for %q is missing guidance %q", tt.category, tt.want)
			}
		})
	}
}

func TestUnsetCategoryGetsTheBasePromptOnly(t *testing.T) {
	llm := &stubLLM{response: "a plain reply"}

	if _, err := newEngine(llm).Reply(context.Background(), Input{Message: "hi"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if llm.got.System != systemPrompt {
		t.Error("an unset category should not append any guidance")
	}
}

func TestReplyThatAssertsLawIsDeferredRegardlessOfCategory(t *testing.T) {
	llm := &stubLLM{response: "Article 265 of the Commercial Code requires registration."}

	reply, err := newEngine(llm).Reply(context.Background(), Input{
		Message: "How do I register?", Category: CategoryGeneralKnowledge,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if !reply.Deferred {
		t.Error("a reply that asserts law must be deferred, whatever the category")
	}
	if reply.Text != "" {
		t.Error("a deferred reply's text must not be shown")
	}
}

func TestEmptyMessageIsRejected(t *testing.T) {
	llm := &stubLLM{response: "unused"}

	if _, err := newEngine(llm).Reply(context.Background(), Input{Message: "   "}); err == nil {
		t.Error("an empty message was accepted")
	}
	if llm.calls != 0 {
		t.Error("an empty message reached the model")
	}
}
