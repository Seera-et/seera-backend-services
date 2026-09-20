package business

import (
	"reflect"
	"testing"
)

func intp(n int) *int         { return &n }
func f64p(f float64) *float64 { return &f }
func boolp(b bool) *bool      { return &b }

// testSet mirrors the shape of the real rule set — the same bounds, from the
// same articles — without depending on a database. The numbers are here because
// this is a test fixture asserting the engine's arithmetic; production numbers
// come from business_structures. See the package comment.
func testSet() RuleSet {
	return RuleSet{
		ID:    "set-1",
		Label: "test",
		Structures: []Structure{
			{
				Code: "sole_proprietorship", Name: "Sole proprietorship", Ordinal: 0,
				MinMembers: intp(1), MaxMembers: intp(1),
			},
			{
				Code: "general_partnership", Name: "General partnership", Ordinal: 1,
				Liability: LiabilityUnlimited, MinMembers: intp(2),
			},
			{
				Code: "share_company", Name: "Share company", Ordinal: 2,
				Liability: LiabilityLimited, PublicShares: boolp(true),
				// Both member bounds unknown, exactly as in the real set: the
				// Code presupposes a minimum (Art. 473/1/a) without stating it.
			},
			{
				Code: "plc", Name: "Private limited company", Ordinal: 3,
				Liability:  LiabilityLimited,
				MinMembers: intp(2), MaxMembers: intp(50),
				MinCapital: f64p(15000), Currency: "ETB",
				PublicShares: boolp(false),
			},
			{
				Code: "one_person_plc", Name: "One person private limited company", Ordinal: 4,
				Liability:  LiabilityLimited,
				MinMembers: intp(1), MaxMembers: intp(1),
				MinCapital: f64p(15000), Currency: "ETB",
				PublicShares: boolp(false),
			},
		},
		Activities: []Activity{
			{Code: "retail_wholesale", Name: "Retail", Ordinal: 0},
			{Code: "financial_services", Name: "Finance", Regulated: true, Ordinal: 1},
		},
		Requirements: []Requirement{
			{Kind: RequirementStep, Title: "Register", Ordinal: 0},
			{Kind: RequirementStep, StructureCode: "plc", Title: "Name ends with PLC", Ordinal: 1},
			{Kind: RequirementStep, StructureCode: "share_company", Title: "Subscribe capital", Ordinal: 2},
			{Kind: RequirementLicence, ActivityCode: "financial_services", Title: "NBE licence", Ordinal: 0},
			{Kind: RequirementCaveat, Title: "Not legal advice", Ordinal: 0},
			{Kind: RequirementCaveat, StructureCode: "share_company", Title: "Minimum unstated", Ordinal: 1},
		},
	}
}

// eligibleCodes returns the eligible structures in the order Evaluate ranked
// them.
func eligibleCodes(candidates []Candidate) []string {
	var out []string
	for _, c := range candidates {
		if c.Eligible {
			out = append(out, c.Structure.Code)
		}
	}
	return out
}

func excludedCodes(candidates []Candidate) []string {
	var out []string
	for _, c := range candidates {
		if !c.Eligible {
			out = append(out, c.Structure.Code)
		}
	}
	return out
}

func hasReason(c Candidate, code ReasonCode) bool {
	for _, r := range c.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func candidate(t *testing.T, candidates []Candidate, code string) Candidate {
	t.Helper()
	for _, c := range candidates {
		if c.Structure.Code == code {
			return c
		}
	}
	t.Fatalf("no candidate %q in %v", code, eligibleCodes(candidates))
	return Candidate{}
}

func TestEvaluateFounderCount(t *testing.T) {
	tests := []struct {
		name         string
		founders     int
		wantEligible []string
		wantExcluded []string
	}{
		{
			name:     "one founder rules out every form needing two",
			founders: 1,
			// share_company survives: its minimum is unknown, and an unknown
			// bound must not disqualify.
			wantEligible: []string{"one_person_plc", "sole_proprietorship", "share_company"},
			wantExcluded: []string{"general_partnership", "plc"},
		},
		{
			name:         "two founders rule out the single-member forms",
			founders:     2,
			wantEligible: []string{"plc", "general_partnership", "share_company"},
			wantExcluded: []string{"sole_proprietorship", "one_person_plc"},
		},
		{
			name:         "fifty-one founders exceed a private limited company",
			founders:     51,
			wantEligible: []string{"general_partnership", "share_company"},
			wantExcluded: []string{"sole_proprietorship", "plc", "one_person_plc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(Intake{Founders: tt.founders}, testSet())

			if eligible := eligibleCodes(got); !sameSet(eligible, tt.wantEligible) {
				t.Errorf("eligible = %v, want %v", eligible, tt.wantEligible)
			}
			if excluded := excludedCodes(got); !sameSet(excluded, tt.wantExcluded) {
				t.Errorf("excluded = %v, want %v", excluded, tt.wantExcluded)
			}
		})
	}
}

func TestEvaluateRanksTheBestFitFirst(t *testing.T) {
	// A single founder wanting limited liability: the one-person company fits
	// both facts, where a sole proprietorship fits only the count.
	got := Evaluate(Intake{
		Founders:              1,
		WantsLimitedLiability: boolp(true),
	}, testSet())

	if got[0].Structure.Code != "one_person_plc" {
		t.Errorf("top recommendation is %q, want one_person_plc", got[0].Structure.Code)
	}
	if !got[0].Eligible {
		t.Error("top recommendation is not eligible")
	}
}

func TestEvaluateUnknownMemberBoundIsPermissiveAndSaidSo(t *testing.T) {
	got := Evaluate(Intake{Founders: 1}, testSet())
	share := candidate(t, got, "share_company")

	if !share.Eligible {
		t.Error("share_company was excluded on an unknown minimum; an unverified bound must not disqualify")
	}
	if !hasReason(share, ReasonMemberLimitUnknown) {
		t.Error("share_company is permitted without saying the limit is unestablished")
	}
}

func TestEvaluateCapitalBelowMinimumDisqualifies(t *testing.T) {
	got := Evaluate(Intake{Founders: 2, CapitalBirr: f64p(5000)}, testSet())
	plc := candidate(t, got, "plc")

	if plc.Eligible {
		t.Error("plc is eligible with capital below its statutory minimum")
	}
	if !hasReason(plc, ReasonCapitalBelowMinimum) {
		t.Errorf("plc excluded without the capital reason: %+v", plc.Reasons)
	}
}

func TestEvaluateCapitalAtTheMinimumQualifies(t *testing.T) {
	// Exactly the minimum is enough: the Code says "may not be less than".
	got := Evaluate(Intake{Founders: 2, CapitalBirr: f64p(15000)}, testSet())
	plc := candidate(t, got, "plc")

	if !plc.Eligible {
		t.Errorf("plc excluded at exactly the minimum: %+v", plc.Reasons)
	}
	if !hasReason(plc, ReasonCapitalSufficient) {
		t.Error("no capital-sufficient reason recorded")
	}
}

func TestEvaluateUnknownCapitalDoesNotDisqualify(t *testing.T) {
	got := Evaluate(Intake{Founders: 2}, testSet())
	plc := candidate(t, got, "plc")

	if !plc.Eligible {
		t.Error("plc excluded because capital was not stated; an unanswered question must not rule a form out")
	}
	if !hasReason(plc, ReasonCapitalUnknown) {
		t.Error("the capital requirement was not surfaced when capital is unknown")
	}
}

func TestEvaluatePublicSubscriptionIsALegalBarNotAPreference(t *testing.T) {
	got := Evaluate(Intake{Founders: 5, RaiseFromPublic: boolp(true)}, testSet())

	plc := candidate(t, got, "plc")
	if plc.Eligible {
		t.Error("plc is eligible while offering shares to the public, which Art. 495/2 forbids")
	}
	if !hasReason(plc, ReasonPublicSharesNotAllowed) {
		t.Error("plc excluded without the public-subscription reason")
	}

	share := candidate(t, got, "share_company")
	if !share.Eligible {
		t.Error("share_company excluded despite being the form that may raise publicly")
	}
	if got[0].Structure.Code != "share_company" {
		t.Errorf("top recommendation is %q, want share_company", got[0].Structure.Code)
	}
}

func TestEvaluateLiabilityPreferenceNeverDisqualifies(t *testing.T) {
	got := Evaluate(Intake{Founders: 2, WantsLimitedLiability: boolp(true)}, testSet())
	partnership := candidate(t, got, "general_partnership")

	if !partnership.Eligible {
		t.Error("general_partnership excluded on a liability preference; a wish is not a legal bar")
	}
	if !hasReason(partnership, ReasonLimitedLiabilityMismatch) {
		t.Error("the mismatch was not explained to the reader")
	}
	// It is lawful but a worse fit, so it must rank below the limited forms.
	if got[0].Structure.Code == "general_partnership" {
		t.Error("general_partnership ranked first despite mismatching the stated preference")
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	intake := Intake{Founders: 3, CapitalBirr: f64p(20000), WantsLimitedLiability: boolp(true)}

	first := Evaluate(intake, testSet())
	for i := range 5 {
		again := Evaluate(intake, testSet())
		if !reflect.DeepEqual(eligibleCodes(first), eligibleCodes(again)) {
			t.Fatalf("run %d ordered candidates differently: %v then %v",
				i, eligibleCodes(first), eligibleCodes(again))
		}
	}
}

func TestEvaluateEveryReasonCarriesItsSources(t *testing.T) {
	// A reason with no citation is a legal claim with no source, which is the
	// failure this package exists to prevent.
	set := testSet()
	for i := range set.Structures {
		set.Structures[i].Sources = []SourceRef{{
			DocumentTitle: "Commercial Code", ArticleNo: "495",
		}}
	}

	for _, c := range Evaluate(Intake{Founders: 2, CapitalBirr: f64p(1)}, set) {
		for _, r := range c.Reasons {
			if len(r.Sources) == 0 {
				t.Errorf("%s reason %q has no source", c.Structure.Code, r.Code)
			}
		}
	}
}

func TestRequirementsAreScopedToTheChoice(t *testing.T) {
	steps, licences, caveats := Requirements(testSet(), "plc", "retail_wholesale")

	if got := titles(steps); !reflect.DeepEqual(got, []string{"Register", "Name ends with PLC"}) {
		t.Errorf("steps = %v, want the global one and the plc one only", got)
	}
	// The licence is scoped to financial services and this business is retail.
	if len(licences) != 0 {
		t.Errorf("licences = %v, want none for an unregulated activity", titles(licences))
	}
	if got := titles(caveats); !reflect.DeepEqual(got, []string{"Not legal advice"}) {
		t.Errorf("caveats = %v, want the global one only", got)
	}
}

func TestRequirementsIncludeActivityScopedLicences(t *testing.T) {
	_, licences, _ := Requirements(testSet(), "plc", "financial_services")

	if got := titles(licences); !reflect.DeepEqual(got, []string{"NBE licence"}) {
		t.Errorf("licences = %v, want the financial-services one", got)
	}
}

func TestRequirementsWithNoStructureStillReturnTheGlobalOnes(t *testing.T) {
	// The state when every form is ruled out: there is no chosen structure, and
	// the honest answer still includes what would have applied regardless.
	steps, _, caveats := Requirements(testSet(), "", "")

	if got := titles(steps); !reflect.DeepEqual(got, []string{"Register"}) {
		t.Errorf("steps = %v, want only the form-independent one", got)
	}
	if got := titles(caveats); !reflect.DeepEqual(got, []string{"Not legal advice"}) {
		t.Errorf("caveats = %v, want only the form-independent one", got)
	}
}

func titles(rs []Requirement) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Title)
	}
	return out
}

// sameSet compares membership, not order: these assertions are about which
// forms are lawful, and ranking is tested separately.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
