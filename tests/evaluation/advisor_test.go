//go:build eval

// Business & License Advisor quality — the gate for SRS AC-003.
//
// AC-003's pass condition is 100% of the predefined scenarios, with no margin.
// That is a fair bar here and nowhere else in this project: the advisor runs no
// model and no similarity ranking, so the same intake against the same rule set
// always produces the same recommendation. A failure is a rule bug or a
// rule-data error, never variance.
//
// It needs a database because the rules live there — the whole point of the
// design is that no threshold is in the Go source. Publish the rule set first:
//
//	set -a && source .env && set +a
//	curl -X POST localhost:8081/api/v1/admin/business/rules \
//	  -H "Authorization: Bearer $ADMIN_API_TOKEN" \
//	  --data-binary @workers/business/rules_v1.json
//	go test -tags eval ./tests/evaluation/... -run TestAdvisor -v
package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/business"
)

func buildAdvisor(t *testing.T) *business.Service {
	t.Helper()
	db := connectDB(t)
	return business.NewService(business.NewRepository(db.Pool))
}

func toIntake(in advisorIntake) business.Intake {
	return business.Intake{
		Founders:              in.Founders,
		ActivityCode:          in.ActivityCode,
		CapitalBirr:           in.CapitalBirr,
		WantsLimitedLiability: in.WantsLimitedLiability,
		RaiseFromPublic:       in.RaiseFromPublic,
		ForeignOwnership:      in.ForeignOwnership,
	}
}

func TestAdvisorScenarios(t *testing.T) {
	dataset, err := LoadAdvisorDataset()
	if err != nil {
		t.Fatalf("advisor dataset: %v", err)
	}
	advisor := buildAdvisor(t)

	var passed int
	for _, sc := range dataset.Scenarios {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		start := time.Now()
		rec, err := advisor.Advise(ctx, toIntake(sc.Intake))
		elapsed := time.Since(start)
		cancel()

		if err != nil {
			if errors.Is(err, business.ErrNoRuleSet) {
				t.Skip("no published business rule set; load workers/business/rules_v1.json first")
			}
			t.Errorf("%s: %v", sc.ID, err)
			continue
		}

		if failures := checkScenario(sc, rec); len(failures) > 0 {
			for _, failure := range failures {
				t.Errorf("%s: %s", sc.ID, failure)
			}
			continue
		}

		passed++
		recommended := "(none eligible)"
		if rec.Recommended != nil {
			recommended = rec.Recommended.Structure.Code
		}
		t.Logf("PASS %s  %-20s %v", sc.ID, recommended, elapsed.Round(time.Millisecond))
	}

	t.Logf("")
	t.Logf("Business Advisor — AC-003")
	t.Logf("  scenarios  %d", len(dataset.Scenarios))
	t.Logf("  passed     %d (%.0f%%, target 100%%)",
		passed, pct(passed, len(dataset.Scenarios)))

	if passed != len(dataset.Scenarios) {
		t.Errorf("%d of %d scenarios passed; AC-003 requires all of them",
			passed, len(dataset.Scenarios))
	}
}

// checkScenario returns every way the recommendation failed the scenario.
//
// All of them, not the first: when a rule change breaks something, the useful
// output is the full picture of what moved, not one assertion at a time.
func checkScenario(sc advisorScenario, rec business.Recommendation) []string {
	var failures []string

	if sc.ExpectRecommended != "" {
		switch {
		case rec.Recommended == nil:
			failures = append(failures,
				"no structure was recommended, want "+sc.ExpectRecommended)
		case rec.Recommended.Structure.Code != sc.ExpectRecommended:
			failures = append(failures, "recommended "+
				rec.Recommended.Structure.Code+", want "+sc.ExpectRecommended)
		}
	}

	excluded := map[string]bool{}
	for _, c := range rec.Excluded {
		excluded[c.Structure.Code] = true
	}
	for _, code := range sc.ExpectExcluded {
		if !excluded[code] {
			failures = append(failures, code+" should have been ruled out and was not")
		}
	}
	for _, code := range sc.ExpectNotExcluded {
		if excluded[code] {
			failures = append(failures, code+
				" was ruled out; nothing in this intake makes it unlawful")
		}
	}

	codes := reasonCodes(rec)
	for _, want := range sc.ExpectReasonCodes {
		if !codes[want] {
			failures = append(failures, "no reason with code "+want)
		}
	}

	for _, want := range sc.ExpectStepsInclude {
		if !containsTitle(rec.Steps, want) {
			failures = append(failures, "step missing: "+want)
		}
	}
	for _, unwanted := range sc.ExpectStepsExclude {
		if containsTitle(rec.Steps, unwanted) {
			failures = append(failures, "step present but should not be: "+unwanted)
		}
	}
	for _, want := range sc.ExpectCaveatsInclude {
		if !containsTitle(rec.Caveats, want) {
			failures = append(failures, "caveat missing: "+want)
		}
	}

	if sc.ExpectNoLicences && len(rec.Licences) > 0 {
		failures = append(failures,
			"a licence was asserted for a sector whose licensing law is not indexed")
	}

	for _, fragment := range sc.ExpectUnsupportedMentions {
		if !mentions(rec.Unsupported, fragment) {
			failures = append(failures,
				"the response does not say it cannot advise on "+fragment)
		}
	}

	// Cross-cutting, asserted on every scenario rather than per case: this is
	// the property the whole design exists for, and a rule that lost its
	// citation should fail whichever scenario happens to reach it.
	failures = append(failures, checkCitations(rec)...)

	return failures
}

// checkCitations verifies that every legal statement carries a source that
// resolved against the corpus.
func checkCitations(rec business.Recommendation) []string {
	var failures []string

	candidates := append([]business.Candidate{}, rec.Alternatives...)
	candidates = append(candidates, rec.Excluded...)
	if rec.Recommended != nil {
		candidates = append(candidates, *rec.Recommended)
	}

	for _, c := range candidates {
		for _, reason := range c.Reasons {
			if len(reason.Sources) == 0 {
				failures = append(failures, string(reason.Code)+" on "+
					c.Structure.Code+" cites nothing")
				continue
			}
			for _, src := range reason.Sources {
				// Empty text means the cited article did not resolve in the
				// published corpus — the rule is live but no longer checkable.
				if strings.TrimSpace(src.Text) == "" {
					failures = append(failures, string(reason.Code)+" on "+
						c.Structure.Code+" cites "+src.Cite()+", which did not resolve")
				}
			}
		}
	}

	// Steps and licences are legal obligations. Caveats are exempt: "no
	// investment legislation is indexed" is a statement about the index, with
	// no article behind it by nature.
	for _, group := range [][]business.Requirement{rec.Steps, rec.Licences} {
		for _, req := range group {
			if len(req.Sources) == 0 {
				failures = append(failures, string(req.Kind)+" \""+req.Title+"\" cites nothing")
			}
		}
	}

	return failures
}

func reasonCodes(rec business.Recommendation) map[string]bool {
	out := map[string]bool{}
	groups := append([]business.Candidate{}, rec.Alternatives...)
	groups = append(groups, rec.Excluded...)
	if rec.Recommended != nil {
		groups = append(groups, *rec.Recommended)
	}
	for _, c := range groups {
		for _, r := range c.Reasons {
			out[string(r.Code)] = true
		}
	}
	return out
}

func containsTitle(reqs []business.Requirement, title string) bool {
	for _, r := range reqs {
		if r.Title == title {
			return true
		}
	}
	return false
}

func mentions(lines []string, fragment string) bool {
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}
