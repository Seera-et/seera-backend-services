package rag

import (
	"context"
	"strings"
	"testing"
)

func TestExtractClaims(t *testing.T) {
	cited := []Citation{
		{Marker: "S1", Text: "Capital must be fully paid before registration."},
		{Marker: "S2", Text: "A private limited company needs at least one member."},
	}

	answer := "The capital must be fully paid before the company is registered [S1]. " +
		"It needs at least one member [S2]."

	claims := extractClaims(answer, cited)
	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2: %+v", len(claims), claims)
	}
	if claims[0].Marker != "S1" || !strings.Contains(claims[0].Text, "capital must be fully paid") {
		t.Errorf("claim[0] = %+v", claims[0])
	}
	if claims[0].Source != cited[0].Text {
		t.Errorf("claim[0].Source = %q, want the cited chunk's text", claims[0].Source)
	}
	if claims[1].Marker != "S2" || !strings.Contains(claims[1].Text, "at least one member") {
		t.Errorf("claim[1] = %+v", claims[1])
	}
}

func TestExtractClaimsSplitsACompoundMarkerIntoOneClaimPerSource(t *testing.T) {
	cited := []Citation{
		{Marker: "S1", Text: "source one"},
		{Marker: "S2", Text: "source two"},
	}
	answer := "Both provisions require registration [S1, S2]."

	claims := extractClaims(answer, cited)
	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2 (one per source in the compound marker): %+v", len(claims), claims)
	}
	markers := map[string]bool{claims[0].Marker: true, claims[1].Marker: true}
	if !markers["S1"] || !markers["S2"] {
		t.Errorf("markers = %v, want both S1 and S2", markers)
	}
}

func TestExtractClaimsKeepsOnlyTheFirstOccurrenceOfARepeatedMarker(t *testing.T) {
	cited := []Citation{{Marker: "S1", Text: "source"}}
	answer := "First statement [S1]. Second, different statement [S1]."

	claims := extractClaims(answer, cited)
	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1 (dedup by marker): %+v", len(claims), claims)
	}
	if !strings.Contains(claims[0].Text, "First statement") {
		t.Errorf("claim = %+v, want the first occurrence's text", claims[0])
	}
}

func TestExtractClaimsSkipsAMarkerWithNoMatchingCitation(t *testing.T) {
	// Cannot happen for an answer MarkerFilter has already validated, but
	// must not panic or invent a claim with an empty source if it somehow did.
	claims := extractClaims("A claim [S9].", nil)
	if len(claims) != 0 {
		t.Errorf("got %d claims, want 0 for an unresolvable marker", len(claims))
	}
}

func TestExtractClaimsIgnoresTextWithNoMarkerAtAll(t *testing.T) {
	claims := extractClaims("An answer with no citations at all.", []Citation{{Marker: "S1", Text: "x"}})
	if len(claims) != 0 {
		t.Errorf("got %d claims, want 0", len(claims))
	}
}

func entailmentClaims() []Claim {
	return []Claim{
		{Marker: "S1", Text: "Capital must be fully paid.", Source: "Capital shall be paid in full before registration."},
		{Marker: "S2", Text: "The company needs three directors.", Source: "A one member company needs no board."},
	}
}

func TestLLMVerifierReturnsVerdictsInOrder(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"verdicts":[true,false]}`}}
	v := NewLLMVerifier(llm)

	got, err := v.Verify(context.Background(), entailmentClaims())
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Errorf("verdicts = %v, want [true false]", got)
	}
}

func TestLLMVerifierSendsAStructuredRequest(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"verdicts":[true,false]}`}}
	v := NewLLMVerifier(llm)

	if _, err := v.Verify(context.Background(), entailmentClaims()); err != nil {
		t.Fatalf("Verify() = %v", err)
	}

	if llm.got.JSONSchema == nil {
		t.Error("verify call did not constrain the response shape")
	}
	if !strings.Contains(llm.got.Messages[0].Content, "Capital must be fully paid.") ||
		!strings.Contains(llm.got.Messages[0].Content, "Capital shall be paid in full before registration.") {
		t.Errorf("prompt missing claim/source text: %q", llm.got.Messages[0].Content)
	}
}

func TestLLMVerifierHandlesEmptyClaims(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"verdicts":[]}`}}
	v := NewLLMVerifier(llm)

	got, err := v.Verify(context.Background(), nil)
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for no claims", got)
	}
}

// A wrong count is exactly as dangerous here as it is for LLMReranker: a
// verdict silently attached to the wrong claim. Service.verifyCitations
// already treats any error as "skip the check," so rejecting outright is the
// safe failure, not a fatal one for the answer.
func TestLLMVerifierRejectsAWrongVerdictCount(t *testing.T) {
	llm := &stubLLM{chunks: []string{`{"verdicts":[true]}`}} // 1 verdict, 2 claims
	v := NewLLMVerifier(llm)

	if _, err := v.Verify(context.Background(), entailmentClaims()); err == nil {
		t.Error("Verify() accepted a mismatched verdict count")
	}
}

func TestLLMVerifierRejectsMalformedJSON(t *testing.T) {
	llm := &stubLLM{chunks: []string{"not json"}}
	v := NewLLMVerifier(llm)

	if _, err := v.Verify(context.Background(), entailmentClaims()); err == nil {
		t.Error("Verify() accepted a malformed response")
	}
}

func TestLLMVerifierSurfacesProviderErrors(t *testing.T) {
	llm := &stubLLM{err: context.DeadlineExceeded}
	v := NewLLMVerifier(llm)

	if _, err := v.Verify(context.Background(), entailmentClaims()); err == nil {
		t.Error("Verify() ignored a provider error")
	}
}

func TestAnswerRecordsUnverifiedCitationsWhenEntailmentIsEnabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CitationEntailment = true

	verifierLLM := &stubLLM{chunks: []string{`{"verdicts":[false]}`}}
	svc := NewService(
		&stubRetriever{results: testResults(1)},
		PassthroughReranker{},
		&stubLLM{chunks: []string{"A company must register [S1]."}},
		cfg, testLogger(),
		WithVerifier(NewLLMVerifier(verifierLLM)),
	)

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if done.Result == nil || len(done.Result.Unverified) != 1 || done.Result.Unverified[0] != "S1" {
		t.Errorf("Result.Unverified = %+v, want [S1]", done.Result)
	}
}

func TestAnswerLeavesUnverifiedNilWhenEntailmentIsDisabled(t *testing.T) {
	// A verifier is wired, but Config.CitationEntailment is left at its
	// DefaultConfig zero value (false) — the flag must gate it, not merely
	// whether a verifier happens to be configured.
	verifierLLM := &stubLLM{chunks: []string{`{"verdicts":[false]}`}}
	svc := NewService(
		&stubRetriever{results: testResults(1)},
		PassthroughReranker{},
		&stubLLM{chunks: []string{"A company must register [S1]."}},
		DefaultConfig(), testLogger(),
		WithVerifier(NewLLMVerifier(verifierLLM)),
	)

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if done.Result != nil && done.Result.Unverified != nil {
		t.Errorf("Unverified = %v, want nil when the flag is off", done.Result.Unverified)
	}
	if verifierLLM.got.Messages != nil {
		t.Error("verifier was called despite the flag being off")
	}
}

func TestAnswerSkipsVerificationWhenNoVerifierIsWired(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CitationEntailment = true // flag on, but no WithVerifier — must not panic

	svc := newTestService(
		&stubRetriever{results: testResults(1)},
		&stubLLM{chunks: []string{"A company must register [S1]."}},
		PassthroughReranker{}, cfg)

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}
	done := events[len(events)-1]
	if done.Result != nil && done.Result.Unverified != nil {
		t.Errorf("Unverified = %v, want nil with no verifier wired", done.Result.Unverified)
	}
}

func TestAnswerVerificationFailureIsSwallowed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CitationEntailment = true

	verifierLLM := &stubLLM{err: context.DeadlineExceeded}
	svc := NewService(
		&stubRetriever{results: testResults(1)},
		PassthroughReranker{},
		&stubLLM{chunks: []string{"A company must register [S1]."}},
		cfg, testLogger(),
		WithVerifier(NewLLMVerifier(verifierLLM)),
	)

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v, want the answer to succeed despite the verifier failing", err)
	}
	done := events[len(events)-1]
	if done.Result != nil && done.Result.Unverified != nil {
		t.Errorf("Unverified = %v, want nil when verification itself errors", done.Result.Unverified)
	}
}

func TestLLMVerifierCapsClaimsJudged(t *testing.T) {
	many := make([]Claim, entailmentMaxClaims+5)
	verdicts := make([]string, len(many))
	for i := range many {
		many[i] = Claim{Marker: "S1", Text: "x", Source: "y"}
		verdicts[i] = "true"
	}
	llm := &stubLLM{chunks: []string{"{\"verdicts\":[" + strings.Join(verdicts[:entailmentMaxClaims], ",") + "]}"}}
	v := NewLLMVerifier(llm)

	got, err := v.Verify(context.Background(), many)
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if len(got) != entailmentMaxClaims {
		t.Errorf("judged %d claims, want the cap of %d", len(got), entailmentMaxClaims)
	}
}
