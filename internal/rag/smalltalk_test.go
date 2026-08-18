package rag

import (
	"strings"
	"testing"
)

// The asymmetry this file exists to protect: a greeting sent to retrieval is an
// awkward "not found", but a legal question sent to a canned reply is an answer
// about Ethiopian law that no source supports. Everything unclear must fall
// through to IntentLegal.
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		text string
		want Intent
	}{
		{"hello", "Hello", IntentGreeting},
		{"hello with punctuation", "Hello!!", IntentGreeting},
		{"padded and capitalised", "  HI there  ", IntentGreeting},
		{"good morning", "Good morning", IntentGreeting},
		{"amharic greeting", "ሰላም", IntentGreeting},
		{"thanks", "Thank you!", IntentThanks},
		{"amharic thanks", "አመሰግናለሁ", IntentThanks},
		{"farewell", "bye", IntentFarewell},

		{"capability", "What can you do?", IntentCapability},
		{"capability phrased differently", "how does this work?", IntentCapability},
		{"identity", "Who are you?", IntentCapability},
		{"provenance", "where do your answers come from", IntentCapability},
		{"lawyer question", "Are you a lawyer?", IntentCapability},

		// Everything below must reach retrieval.
		{"plain legal question", "How do I register a company?", IntentLegal},
		{"greeting attached to a question", "Hi, what does Article 5 say?", IntentLegal},
		{"thanks attached to a question", "Thanks — what is the notice period?", IntentLegal},
		{"capability phrase inside a legal question",
			"What can you do about a breach of Article 12 of the Commercial Code, and what remedies exist for the injured party?",
			IntentLegal},
		{"a question containing hi", "Which article covers hiring an employee?", IntentLegal},
		{"amharic legal question", "የንግድ ማኅበር እንዴት ይቋቋማል?", IntentLegal},
		{"empty", "   ", IntentLegal},
		{"article lookup", "Article 627", IntentLegal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.text); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestConversationalReplyMakesNoLegalClaim(t *testing.T) {
	for _, intent := range []Intent{
		IntentGreeting, IntentThanks, IntentFarewell, IntentCapability,
	} {
		reply := ConversationalReply(intent)
		if strings.TrimSpace(reply) == "" {
			t.Fatalf("%s has no reply", intent)
		}

		// These replies are written, not generated, and they describe the product.
		// A sentence stating a rule of Ethiopian law has no business in one — it
		// would be the only text in the system that reaches a user uncited.
		for _, forbidden := range []string{"Article ", "Proclamation", "shall be", "must be paid"} {
			if strings.Contains(reply, forbidden) {
				t.Errorf("%s reply contains %q, which reads as a legal claim:\n%s",
					intent, forbidden, reply)
			}
		}
	}
}

func TestConversationalReplyIsEmptyForLegal(t *testing.T) {
	// A legal question must never be answerable from this path.
	if got := ConversationalReply(IntentLegal); got != "" {
		t.Errorf("ConversationalReply(IntentLegal) = %q, want empty", got)
	}
}

func TestCapabilityReplyStatesTheLimits(t *testing.T) {
	reply := ConversationalReply(IntentCapability)

	for _, expected := range []string{"not a lawyer", "guess", "Amharic"} {
		if !strings.Contains(reply, expected) {
			t.Errorf("capability reply should mention %q:\n%s", expected, reply)
		}
	}
}

// The service-level behaviour: a greeting never reaches retrieval or the model.
func TestAnswerHandlesAGreetingWithoutRetrieval(t *testing.T) {
	retriever := &stubRetriever{results: testResults(3)}
	llm := &scriptedLLM{responses: [][]string{{"should not be called"}}}
	svc := newTestService(retriever, llm, PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "Hello"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if retriever.got.Text != "" {
		t.Errorf("retrieval ran for a greeting, searching for %q", retriever.got.Text)
	}
	if len(llm.requests) != 0 {
		t.Errorf("made %d model calls for a greeting, want none", len(llm.requests))
	}

	done := events[len(events)-1]
	if done.Type != EventDone {
		t.Fatalf("last event is %q, want done", done.Type)
	}
	if done.Result.Kind != KindConversation {
		t.Errorf("kind = %q, want %q", done.Result.Kind, KindConversation)
	}
	// Neither is true of a greeting, and a client reading only these must not be
	// able to present it as a grounded legal answer.
	if done.Result.Abstained || done.Result.Grounded {
		t.Errorf("abstained=%v grounded=%v, want both false",
			done.Result.Abstained, done.Result.Grounded)
	}
	if len(done.Result.Citations) != 0 {
		t.Errorf("a conversational reply carries %d citations, want none",
			len(done.Result.Citations))
	}

	var answer strings.Builder
	for _, e := range events {
		if e.Type == EventToken {
			answer.WriteString(e.Text)
		}
	}
	if !strings.Contains(answer.String(), "Ethiopian law") {
		t.Errorf("greeting reply does not say what Seera is for:\n%s", answer.String())
	}
	// The old behaviour, which is what prompted this path.
	if strings.Contains(answer.String(), AbstainMessage) {
		t.Error("a greeting was answered with the abstention message")
	}
}

func TestAnswerStillRetrievesForALegalQuestion(t *testing.T) {
	retriever := &stubRetriever{results: testResults(2)}
	llm := &scriptedLLM{responses: [][]string{{"An answer [S1]."}}}
	svc := newTestService(retriever, llm, PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "Hi, what does Article 5 say?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	if retriever.got.Text != "Hi, what does Article 5 say?" {
		t.Errorf("retrieval searched for %q, want the question", retriever.got.Text)
	}
	if kind := events[len(events)-1].Result.Kind; kind != KindLegal {
		t.Errorf("kind = %q, want %q", kind, KindLegal)
	}
}

func TestAbstentionIsTaggedAsSuch(t *testing.T) {
	svc := newTestService(
		&stubRetriever{results: nil},
		&scriptedLLM{},
		PassthroughReranker{}, DefaultConfig())

	events, err := collect(t, svc, Question{Text: "What is the law on space travel?"})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	result := events[len(events)-1].Result
	if result.Kind != KindAbstention {
		t.Errorf("kind = %q, want %q", result.Kind, KindAbstention)
	}
	if !result.Abstained {
		t.Error("abstained = false on an abstention")
	}
}
