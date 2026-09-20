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

// Same reasoning as the routing dataset: a malformed Explorer set should fail
// in the ordinary suite, not only when someone runs the tagged evaluation
// against a live corpus.
func TestExplorerDatasetIsWellFormed(t *testing.T) {
	dataset, err := LoadExplorerDataset()
	if err != nil {
		t.Fatalf("explorer dataset: %v", err)
	}

	// SRS §24 asks for 20 Knowledge Explorer queries.
	if len(dataset.Cases) < 20 {
		t.Errorf("dataset has %d cases, want at least the 20 the SRS specifies",
			len(dataset.Cases))
	}

	var (
		exact    int // deterministic article-number lookups
		semantic int // phrased without the provision's vocabulary
		empty    int // out of corpus
	)
	for _, tc := range dataset.Cases {
		if tc.ExpectRank > 0 {
			exact++
		}
		if tc.Semantic {
			semantic++
		}
		if tc.ExpectEmpty {
			empty++
		}

		for _, number := range tc.ExpectArticles {
			if strings.TrimSpace(number) != number || number == "" {
				t.Errorf("case %q has a padded or empty expected article %q", tc.ID, number)
			}
		}
		if !tc.ExpectEmpty && !tc.Adjacent && len(tc.ExpectDocuments) == 0 {
			t.Errorf("case %q names articles but no document; article numbers repeat across codes",
				tc.ID)
		}
	}

	// The mix is the point. A set of only exact lookups would score well and
	// measure nothing about search; a set with no out-of-corpus case cannot
	// tell a working ranking from one that always returns something.
	if exact < 3 {
		t.Errorf("only %d deterministic article-number cases; the exact-lookup path is the one AC-002 names", exact)
	}
	if semantic < 3 {
		t.Errorf("only %d plain-language cases; without them the semantic arm is unmeasured", semantic)
	}
	if empty == 0 {
		t.Error("no out-of-corpus case: nothing checks that an unanswerable query returns nothing")
	}
}

func TestAdvisorDatasetIsWellFormed(t *testing.T) {
	dataset, err := LoadAdvisorDataset()
	if err != nil {
		t.Fatalf("advisor dataset: %v", err)
	}

	// SRS §24 asks for 10 Business Advisor scenarios.
	if len(dataset.Scenarios) < 10 {
		t.Errorf("dataset has %d scenarios, want at least the 10 the SRS specifies",
			len(dataset.Scenarios))
	}

	var withRecommendation, withExclusion int
	for _, sc := range dataset.Scenarios {
		if sc.ExpectRecommended != "" {
			withRecommendation++
		}
		if len(sc.ExpectExcluded) > 0 {
			withExclusion++
		}
		if strings.TrimSpace(sc.Note) == "" {
			// Every scenario asserts something about Ethiopian law. Without a
			// note saying which provision makes it true, a later reader cannot
			// tell a deliberate expectation from a recorded bug.
			t.Errorf("scenario %q has no note explaining why its expectation is correct", sc.ID)
		}
	}

	// A set that only ever checks the happy path would not catch the rules that
	// matter most — the ones that rule a form out.
	if withRecommendation < 4 {
		t.Errorf("only %d scenarios assert a recommendation", withRecommendation)
	}
	if withExclusion < 4 {
		t.Errorf("only %d scenarios assert an exclusion; disqualification is where the legal thresholds bite",
			withExclusion)
	}
}
