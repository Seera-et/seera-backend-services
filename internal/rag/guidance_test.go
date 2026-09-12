package rag

import (
	"testing"
)

// Rule 12 of SystemPrompt permits a general-guidance section only after the
// model has written NOT_IN_SOURCES. These pin what happens when it does not.
//
// Observed live 2026-09-03: an answer citing Articles 495, 496 and 499 on
// private limited companies appended two sentences of unsourced company-law
// commentary under the heading, and was shown to the reader badged "Grounded".
// len(used) > 0 held, so nothing in the grounding check noticed.

func TestUnsourcedGuidanceOnAnsweredQuestionIsNotGrounded(t *testing.T) {
	llm := &stubLLM{chunks: []string{
		"A private limited company needs fully paid capital [S1].\n\n",
		GeneralGuidanceHeading,
		"\nTypically such requirements exist to protect creditors. Verify this.",
	}}

	svc := NewService(&stubRetriever{results: testResults(2)},
		PassthroughReranker{}, llm, DefaultConfig(), testLogger())

	events, err := collect(t, svc, Question{Text: "what makes a company a PLC?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1].Result
	if done.Kind != KindLegal {
		t.Fatalf("kind = %q, want %q — the model answered from the sources", done.Kind, KindLegal)
	}
	if done.Grounded {
		t.Error("an answer carrying unsourced general guidance was reported as grounded")
	}
}

// The legitimate case must keep working: after NOT_IN_SOURCES the section is
// exactly what rule 12 asks for, and the answer is not claiming to be grounded
// legal reasoning in the first place.
func TestGuidanceAfterInsufficientSourcesStaysGrounded(t *testing.T) {
	llm := &stubLLM{chunks: []string{
		InsufficientSourcesSentinel,
		"\nThe retrieved provisions do not settle this.\n\n",
		GeneralGuidanceHeading,
		"\nGenerally this is handled by contract. Not confirmed Ethiopian law.",
	}}

	svc := NewService(&stubRetriever{results: testResults(2)},
		PassthroughReranker{}, llm, DefaultConfig(), testLogger())

	events, err := collect(t, svc, Question{Text: "how are these usually handled?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1].Result
	if done.Kind != KindInsufficient {
		t.Fatalf("kind = %q, want %q", done.Kind, KindInsufficient)
	}
	if !done.Grounded {
		t.Error("a rule-12-compliant guidance section was penalised")
	}
}

// An ordinary cited answer with no guidance section is unaffected — the check
// must not cost groundedness on the overwhelmingly common path.
func TestOrdinaryCitedAnswerStaysGrounded(t *testing.T) {
	svc := NewService(&stubRetriever{results: testResults(2)},
		PassthroughReranker{}, &stubLLM{chunks: []string{"Capital must be fully paid [S1]."}},
		DefaultConfig(), testLogger())

	events, err := collect(t, svc, Question{Text: "what makes a company a PLC?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	done := events[len(events)-1].Result
	if !done.Grounded {
		t.Error("a normal cited answer was reported as ungrounded")
	}
}
