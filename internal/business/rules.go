package business

import (
	"fmt"
	"sort"
)

// Scoring weights.
//
// These are product judgements about which eligible form to put first, not
// legal facts, which is why they are allowed to live in code where a capital
// threshold is not. They only ever order candidates that are all lawful.
const (
	// Being able to do the specific thing the founders said they need. The
	// strongest signal by design: raising capital publicly is something they
	// asked for *and* a capability the law grants or withholds, so a form that
	// permits it should beat one that merely fits the founder count.
	//
	// This ordering was not obvious. With it below scoreMembersFit, a query for
	// five founders wanting public subscription recommended a general
	// partnership — whose own public-subscription status the corpus does not
	// establish, so it was never penalised — over the share company, which is
	// the form for exactly that. Ranking "nothing rules this out" above "this
	// does what you asked" is the wrong way round.
	scorePublicSharesFit = 50

	// A form whose member range is fully known to fit beats one that merely
	// cannot be ruled out: "this is the form for two founders" is a stronger
	// statement than "nothing indexed says otherwise".
	scoreMembersFit = 40

	// Matching a stated liability preference.
	scoreLiabilityMatch = 30

	// Only one bound is known and the founder count clears it. Worth less than
	// a fully-known range, because the other end is still unverified.
	scoreMembersPartialFit = 20

	// Capital the founders actually have.
	scoreCapitalFit = 10

	// Breaks ties toward the order the rule set lists forms in, which is the
	// order the Code enumerates them (Art. 174).
	scoreOrdinalWeight = 1
)

// Evaluate assesses every structure in the set against the intake.
//
// Pure: no database, no network, no model, no clock. This is the function
// AC-003 measures — "100% of predefined MVP scenarios produce the expected
// rule-based recommendation" — so it has to be decidable by reading it, and
// testable without any of the infrastructure around it.
//
// Two principles run through it:
//
//   - An unknown bound never disqualifies. nil means the indexed corpus does
//     not establish the limit, and silently dropping a lawful option is worse
//     than offering one with an explicit caveat. Same reasoning as
//     legal.Version.InForceAt's treatment of an unknown effective date.
//   - A preference never disqualifies. Wanting limited liability is not a legal
//     constraint, so it moves a form up the list and never off it.
func Evaluate(intake Intake, set RuleSet) []Candidate {
	candidates := make([]Candidate, 0, len(set.Structures))

	for _, structure := range set.Structures {
		candidate := Candidate{Structure: structure, Eligible: true}

		candidate.assess(memberReason(intake, structure))
		candidate.assess(capitalReason(intake, structure))
		candidate.assess(publicSharesReason(intake, structure))
		candidate.assess(liabilityReason(intake, structure))

		candidate.Score += structureOrdinalScore(structure, len(set.Structures))
		candidates = append(candidates, candidate)
	}

	// Eligible first, then by score, then by the rule set's own order. Stable so
	// an identical intake always produces an identical ordering — a
	// recommendation that reshuffles between requests is not reproducible.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Eligible != candidates[j].Eligible {
			return candidates[i].Eligible
		}
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Structure.Ordinal < candidates[j].Structure.Ordinal
	})

	return candidates
}

// assess folds one finding into the candidate.
func (c *Candidate) assess(reason *Reason, score int) {
	if reason == nil {
		return
	}
	if reason.Disqualifying {
		c.Eligible = false
	}
	c.Score += score
	c.Reasons = append(c.Reasons, *reason)
}

// memberReason decides whether the founder count fits the form.
//
// This is the most discriminating rule in the set: "two of us" rules out a one
// member private limited company and a sole proprietorship outright, and
// "sixty of us" rules out a private limited company (Commercial Code Art.
// 495/4). The numbers come from the structure row; none is written here.
func memberReason(intake Intake, s Structure) (*Reason, int) {
	switch {
	case s.MinMembers != nil && intake.Founders < *s.MinMembers:
		return &Reason{
			Code:          ReasonMembersBelowMinimum,
			Disqualifying: true,
			Message: fmt.Sprintf("%s needs at least %s, and you have %s.",
				s.Name, people(*s.MinMembers), people(intake.Founders)),
			Sources: sourcesFor(s.Sources, FacetMembers),
		}, 0

	case s.MaxMembers != nil && intake.Founders > *s.MaxMembers:
		return &Reason{
			Code:          ReasonMembersAboveMaximum,
			Disqualifying: true,
			Message: fmt.Sprintf("%s may not have more than %s, and you have %s.",
				s.Name, people(*s.MaxMembers), people(intake.Founders)),
			Sources: sourcesFor(s.Sources, FacetMembers),
		}, 0

	case s.MinMembers != nil && s.MaxMembers != nil:
		return &Reason{
			Code:    ReasonMembersWithinRange,
			Message: fmt.Sprintf("%s suits %s.", s.Name, people(intake.Founders)),
			Sources: sourcesFor(s.Sources, FacetMembers),
		}, scoreMembersFit

	case s.MinMembers != nil:
		// The minimum is known and met; only the ceiling is unstated. Saying
		// "the corpus does not state how many members this requires" here would
		// be false — it states the floor, and the founders clear it.
		return &Reason{
			Code: ReasonMembersWithinRange,
			Message: fmt.Sprintf(
				"%s needs at least %s, which your %s meets. The indexed corpus does not state a maximum.",
				s.Name, people(*s.MinMembers), people(intake.Founders)),
			Sources: sourcesFor(s.Sources, FacetMembers),
		}, scoreMembersPartialFit

	case s.MaxMembers != nil:
		return &Reason{
			Code: ReasonMembersWithinRange,
			Message: fmt.Sprintf(
				"%s allows at most %s, which your %s is within. The indexed corpus does not state a minimum.",
				s.Name, people(*s.MaxMembers), people(intake.Founders)),
			Sources: sourcesFor(s.Sources, FacetMembers),
		}, scoreMembersPartialFit

	default:
		// Neither bound is known. Permissive, and said so — this is the share
		// company's situation, where Art. 473/1/a refers to a statutory minimum
		// number of shareholders that the indexed text never states.
		return &Reason{
			Code: ReasonMemberLimitUnknown,
			Message: fmt.Sprintf(
				"The indexed corpus does not state how many members %s requires, so this has not been checked against your %s.",
				s.Name, people(intake.Founders)),
			Sources: sourcesFor(s.Sources, FacetMembers),
		}, 0
	}
}

// capitalReason compares what the founders have against what the form needs.
func capitalReason(intake Intake, s Structure) (*Reason, int) {
	if s.MinCapital == nil {
		return &Reason{
			Code: ReasonCapitalNotEstablished,
			Message: fmt.Sprintf(
				"The indexed corpus does not state a minimum capital for %s.", s.Name),
			Sources: sourcesFor(s.Sources, FacetGeneral),
		}, 0
	}

	required := formatMoney(*s.MinCapital, s.Currency)

	if intake.CapitalBirr == nil {
		return &Reason{
			Code:    ReasonCapitalUnknown,
			Message: fmt.Sprintf("%s requires capital of at least %s.", s.Name, required),
			Sources: sourcesFor(s.Sources, FacetCapital),
		}, 0
	}

	// Below the statutory minimum is disqualifying: the form cannot lawfully be
	// registered, however much the founders would prefer it.
	if *intake.CapitalBirr < *s.MinCapital {
		return &Reason{
			Code:          ReasonCapitalBelowMinimum,
			Disqualifying: true,
			Message: fmt.Sprintf("%s requires at least %s, and you have %s.",
				s.Name, required, formatMoney(*intake.CapitalBirr, s.Currency)),
			Sources: sourcesFor(s.Sources, FacetCapital),
		}, 0
	}

	return &Reason{
		Code:    ReasonCapitalSufficient,
		Message: fmt.Sprintf("Your capital meets the %s minimum for %s.", required, s.Name),
		Sources: sourcesFor(s.Sources, FacetCapital),
	}, scoreCapitalFit
}

// publicSharesReason handles offering shares to the public, which is a legal
// constraint rather than a preference: a private limited company's shares may
// not be open to public subscription.
func publicSharesReason(intake Intake, s Structure) (*Reason, int) {
	if intake.RaiseFromPublic == nil || s.PublicShares == nil {
		return nil, 0
	}

	if *intake.RaiseFromPublic && !*s.PublicShares {
		return &Reason{
			Code:          ReasonPublicSharesNotAllowed,
			Disqualifying: true,
			Message: fmt.Sprintf(
				"%s may not offer its shares to the public, which is what you said you want to do.",
				s.Name),
			Sources: sourcesFor(s.Sources, FacetPublicShares),
		}, 0
	}

	if *intake.RaiseFromPublic && *s.PublicShares {
		return &Reason{
			Code:    ReasonPublicSharesAllowed,
			Message: fmt.Sprintf("%s may raise capital by public subscription.", s.Name),
			Sources: sourcesFor(s.Sources, FacetPublicShares),
		}, scorePublicSharesFit
	}

	return nil, 0
}

// liabilityReason records a preference. Never disqualifying — a founder who
// would rather not be personally liable has expressed a wish, not a legal bar.
func liabilityReason(intake Intake, s Structure) (*Reason, int) {
	if intake.WantsLimitedLiability == nil || s.Liability == "" {
		return nil, 0
	}

	limited := structureLimitsLiability(s)
	if limited == nil {
		return nil, 0
	}

	if *intake.WantsLimitedLiability == *limited {
		if *limited {
			return &Reason{
				Code:    ReasonLimitedLiabilityMatch,
				Message: fmt.Sprintf("Members of %s are not personally liable for its debts.", s.Name),
				Sources: sourcesFor(s.Sources, FacetLiability),
			}, scoreLiabilityMatch
		}
		return nil, 0
	}

	if *intake.WantsLimitedLiability {
		return &Reason{
			Code: ReasonLimitedLiabilityMismatch,
			Message: fmt.Sprintf(
				"%s does not shield its members from the organisation's debts, and you said you would prefer limited liability.",
				s.Name),
			Sources: sourcesFor(s.Sources, FacetLiability),
		}, 0
	}
	return nil, 0
}

// structureLimitsLiability reads limited liability off the rule set's own
// description of the form.
//
// The flag is derived from the curated Liability text rather than being a
// separate boolean column, so there is one statement of the fact and it is the
// one shown to the reader — a boolean that disagreed with the prose beside it
// would be the worst of both. nil means the text does not settle it.
func structureLimitsLiability(s Structure) *bool {
	switch s.Liability {
	case LiabilityLimited:
		yes := true
		return &yes
	case LiabilityUnlimited:
		no := false
		return &no
	default:
		return nil
	}
}

// The two liability values the rule set may use to make the distinction
// machine-readable. Any other text is treated as "not settled" and simply does
// not participate in scoring.
const (
	LiabilityLimited   = "limited"
	LiabilityUnlimited = "unlimited"
)

// structureOrdinalScore keeps the rule set's ordering as the weakest tiebreak.
func structureOrdinalScore(s Structure, total int) int {
	return (total - s.Ordinal) * scoreOrdinalWeight
}

// Requirements selects what applies to a chosen structure and activity.
//
// A requirement with no structure code applies to every form, and one with no
// activity code applies whatever the business does. Anything scoped to a
// different form or a different activity is left out — an advisor that lists
// every licence in the rule set has not advised anybody.
func Requirements(set RuleSet, structureCode, activityCode string) (steps, licences, caveats []Requirement) {
	// Non-nil from the start: a nil slice marshals to null, and a client
	// reading null where it expected a list treats "no licence needed" and
	// "the field is missing" as the same thing.
	steps, licences, caveats = []Requirement{}, []Requirement{}, []Requirement{}

	for _, r := range set.Requirements {
		if r.StructureCode != "" && r.StructureCode != structureCode {
			continue
		}
		if r.ActivityCode != "" && r.ActivityCode != activityCode {
			continue
		}

		switch r.Kind {
		case RequirementStep:
			steps = append(steps, r)
		case RequirementLicence:
			licences = append(licences, r)
		case RequirementCaveat:
			caveats = append(caveats, r)
		}
	}

	for _, group := range [][]Requirement{steps, licences, caveats} {
		sort.SliceStable(group, func(i, j int) bool { return group[i].Ordinal < group[j].Ordinal })
	}
	return steps, licences, caveats
}

// people renders a count with its noun, so messages read as sentences.
func people(n int) string {
	if n == 1 {
		return "1 founder"
	}
	return fmt.Sprintf("%d founders", n)
}
