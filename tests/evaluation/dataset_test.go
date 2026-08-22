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
