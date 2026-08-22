package evaluation

import (
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/router"
)

// Runs in the ordinary suite, without a provider key. A dataset that no longer
// parses, or that has drifted out of shape, should fail here rather than in the
// tagged evaluation someone runs once a month.
func TestRoutingDatasetIsWellFormed(t *testing.T) {
	dataset, err := LoadRoutingDataset()
	if err != nil {
		t.Fatalf("routing dataset: %v", err)
	}

	if len(dataset.Cases) < 40 {
		t.Errorf("dataset has %d cases; a routing set this small does not say much",
			len(dataset.Cases))
	}

	counts := map[string]int{}
	for _, tc := range dataset.Cases {
		counts[tc.Expect]++

		for _, turn := range tc.History {
			if turn.Role != "user" && turn.Role != "assistant" {
				t.Errorf("case %q has history role %q, want user or assistant",
					tc.ID, turn.Role)
			}
		}

		if tc.ExpectPriorSources && len(tc.PriorSources) == 0 {
			t.Errorf("case %q expects sources to be carried but lists none", tc.ID)
		}
		if len(tc.RewriteMustMention) > 0 && len(tc.History) == 0 {
			t.Errorf("case %q checks a rewrite but has no conversation to rewrite from",
				tc.ID)
		}
	}

	// Both directions have to be represented, or the numbers mean nothing: a set
	// of only legal questions scores 100% on a router that always says "legal".
	for _, intent := range []string{
		string(router.IntentLegal),
		string(router.IntentDiscussion),
		string(router.IntentGeneral),
	} {
		if counts[intent] < 3 {
			t.Errorf("only %d cases expect %q; the set cannot measure that direction",
				counts[intent], intent)
		}
	}

	t.Logf("routing dataset: %d cases (%s)", len(dataset.Cases), summarise(counts))
}

// Runs in the ordinary suite, without a provider key or a database — this only
// checks the dataset parses and has a sane shape. Answer-quality itself needs
// a real corpus and is checked by TestAnswerQuality, behind the eval tag.
func TestAnswerDatasetIsWellFormed(t *testing.T) {
	dataset, err := LoadAnswerDataset()
	if err != nil {
		t.Fatalf("answer dataset: %v", err)
	}

	var (
		amharic       int
		abstain       int
		citationStyle int
		unverified    int
	)
	for _, tc := range dataset.Cases {
		if tc.Language == "am" {
			amharic++
		}
		if tc.ShouldAbstain {
			abstain++
		}
		if len(tc.ExpectedArticleNos) > 0 {
			citationStyle++
		} else if !tc.ShouldAbstain {
			// A case with a document but no article number is real but
			// unverified ground truth — see the comment on answerCase. Worth
			// counting so the gap in coverage is visible, not silent.
			unverified++
		}
	}

	if amharic < 2 {
		t.Errorf("dataset has %d Amharic cases; too few to say anything about that language", amharic)
	}
	if abstain < 3 {
		t.Errorf("dataset has %d should_abstain cases, want at least 3", abstain)
	}

	t.Logf("answer dataset: %d cases (amharic: %d, abstain: %d, citation-style: %d, "+
		"document-only/unverified: %d)", len(dataset.Cases), amharic, abstain, citationStyle, unverified)
	if unverified > 0 {
		t.Logf("%d case(s) have a document title but no verified article number — "+
			"fill these in against the real ingested corpus before trusting citation-accuracy "+
			"results for them; see the answerCase doc comment in dataset.go", unverified)
	}
}

// Runs unconditionally — no provider key, no database. Confirms the
// behavioral dataset parses, every pattern is one this project knows how to
// check, and there is more than a token case per pattern.
func TestEdgeCaseDatasetIsWellFormed(t *testing.T) {
	dataset, err := LoadEdgeCaseDataset()
	if err != nil {
		t.Fatalf("edge case dataset: %v", err)
	}

	counts := map[edgePattern]int{}
	for _, tc := range dataset.Cases {
		counts[tc.Pattern]++
	}

	for pattern := range knownEdgePatterns {
		if counts[pattern] == 0 {
			t.Errorf("no cases for pattern %q — every pattern this project knows how to check should have at least one", pattern)
		}
	}

	t.Logf("edge case dataset: %d cases across %d patterns", len(dataset.Cases), len(counts))
}

func summarise(counts map[string]int) string {
	parts := make([]string, 0, len(counts))
	for _, intent := range []string{"legal", "discussion", "general"} {
		if n := counts[intent]; n > 0 {
			parts = append(parts, intent+": "+itoa(n))
		}
	}
	return strings.Join(parts, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
