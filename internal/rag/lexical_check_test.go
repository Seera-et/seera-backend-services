package rag

import "testing"

func TestLexicallySupported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claim  string
		source string
		want   bool
	}{
		{
			name:   "number present in source",
			claim:  "The minimum capital is 15,000 Birr.",
			source: "The capital of a private limited company may not be less than fifteen thousand (15000) Ethiopian Birr.",
			want:   true,
		},
		{
			name:   "number absent from source",
			claim:  "The minimum capital is 20,000 Birr.",
			source: "The capital of a private limited company may not be less than fifteen thousand (15000) Ethiopian Birr.",
			want:   false,
		},
		{
			name:   "claim has no numbers to check",
			claim:  "A private limited company must have a firm-name.",
			source: "Article 497. A private limited company shall have a firm-name.",
			want:   true,
		},
		{
			name:   "comma formatting does not cause a false mismatch",
			claim:  "The capital must be at least 15000 Birr.",
			source: "may not be less than 15,000 Ethiopian Birr.",
			want:   true,
		},
		{
			name:   "one of several numbers is wrong",
			claim:  "Filed within 90 days, with a 15,000 Birr fee.",
			source: "Filed within 90 days, with a 20,000 Birr fee.",
			want:   false,
		},
		{
			name:   "all numbers present",
			claim:  "Filed within 90 days, with a 15,000 Birr fee.",
			source: "Filed within 90 days, with a 15,000 Birr fee.",
			want:   true,
		},
		{
			// The real, live case found 2026-09-03: Article 496's actual
			// stored text spells its amount out entirely in words, with no
			// digit characters anywhere, but the model correctly restates it
			// with a digit form in its answer. Without word-number parsing
			// this would have been a false positive on a correct citation.
			name:   "digit claim against a source with no digits at all, correctly restated",
			claim:  "The minimum capital required is fifteen thousand (15,000) Ethiopian Birr.",
			source: "Article 496. Capital\n1/ The capital of a private limited company may not be less than fifteen thousand Ethiopian Birr.\n2/ The par value of each share may not be less than one hundred Ethiopian Birr; All shares shall be of equal par value.",
			want:   true,
		},
		{
			name:   "digit claim against a word-only source that actually disagrees",
			claim:  "The minimum capital required is 20,000 Ethiopian Birr.",
			source: "The capital of a private limited company may not be less than fifteen thousand Ethiopian Birr.",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lexicallySupported(tc.claim, tc.source); got != tc.want {
				t.Errorf("lexicallySupported(%q, %q) = %v, want %v", tc.claim, tc.source, got, tc.want)
			}
		})
	}
}

func TestWordNumbersIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want map[string]bool
	}{
		{"simple thousand", "fifteen thousand Ethiopian Birr", map[string]bool{"15000": true}},
		{"bare hundred", "one hundred Ethiopian Birr", map[string]bool{"100": true}},
		{"compound hundred-thousand", "two hundred thirty four thousand Birr", map[string]bool{"234000": true}},
		{"small cardinal, no scale word", "ninety consecutive days", map[string]bool{"90": true}},
		{"two separate runs", "two or more than fifty members", map[string]bool{"2": true, "50": true}},
		{"no number words at all", "a private limited company", map[string]bool{}},
		{"run at the very end of the text", "shall not be less than one hundred", map[string]bool{"100": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wordNumbersIn(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("wordNumbersIn(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for n := range tc.want {
				if !got[n] {
					t.Errorf("wordNumbersIn(%q) missing %q, got %v", tc.text, n, got)
				}
			}
		})
	}
}

func TestNumbersInNormalisesThousandsSeparators(t *testing.T) {
	got := numbersIn("15,000 and 3.5 and 90")
	want := map[string]bool{"15000": true, "3.5": true, "90": true}
	if len(got) != len(want) {
		t.Fatalf("numbersIn() = %v, want %v", got, want)
	}
	for n := range want {
		if !got[n] {
			t.Errorf("numbersIn() missing %q", n)
		}
	}
}

func TestCheckCitationsRunsTheMechanicalCheckWithEntailmentOff(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CitationEntailment = false // the mechanical check must not depend on this flag

	svc := NewService(
		&stubRetriever{results: testResults(1)},
		PassthroughReranker{},
		&stubLLM{chunks: []string{"The fee is 20,000 Birr [S1]."}},
		cfg, testLogger(),
	)

	events, err := collect(t, svc, Question{Text: "fee?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if done.Result == nil || len(done.Result.Unverified) != 1 || done.Result.Unverified[0] != "S1" {
		t.Errorf("Result.Unverified = %+v, want [S1] — the mechanical check should flag a number the source doesn't contain even with the flag off", done.Result)
	}
}

func TestCheckCitationsDoesNotFlagACorrectlyCitedNumber(t *testing.T) {
	cfg := DefaultConfig()

	svc := NewService(
		&stubRetriever{results: testResults(1)},
		PassthroughReranker{},
		&stubLLM{chunks: []string{"A company must register [S1]."}},
		cfg, testLogger(),
	)

	events, err := collect(t, svc, Question{Text: "registration?"})
	if err != nil {
		t.Fatalf("Answer() = %v", err)
	}

	done := events[len(events)-1]
	if done.Result != nil && done.Result.Unverified != nil {
		t.Errorf("Unverified = %v, want nil for a claim with nothing mechanically wrong", done.Result.Unverified)
	}
}
