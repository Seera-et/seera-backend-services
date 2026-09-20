package business

import (
	"context"
	"fmt"
)

// What the advisor cannot speak to on this corpus.
//
// These strings are statements about the *index*, not about the law, which is
// why they are allowed to be here: "no investment legislation is indexed" is a
// fact about this deployment, and the alternative to saying it is letting a
// foreign founder believe silence means permission.
const (
	unsupportedForeignOwnership = "Foreign ownership rules: no investment legislation is indexed, " +
		"so nothing here addresses restrictions on foreign participation, sector caps, " +
		"or minimum foreign capital."
	unsupportedGeneralLicensing = "General trade licensing: no commercial registration and business " +
		"licensing proclamation is indexed. Registration steps below come from the " +
		"Commercial Code only, and the licence your activity needs may not appear at all."
)

// Store is the persistence this service needs.
type Store interface {
	Published(ctx context.Context) (RuleSet, error)
}

// Service answers the advisor's two questions: what to ask, and what follows
// from the answers.
//
// Thin, like documents.Service: the decisions are in rules.go, the citations are
// resolved in the repository, and what is left is assembling a response and
// being honest about its edges.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

// Form returns the questionnaire for the published rule set.
func (s *Service) Form(ctx context.Context) (IntakeForm, error) {
	set, err := s.store.Published(ctx)
	if err != nil {
		return IntakeForm{}, err
	}

	return IntakeForm{
		RuleSetID:    set.ID,
		RuleSetLabel: set.Label,
		Activities:   set.Activities,
		Structures:   set.Structures,
		Unsupported:  []string{unsupportedForeignOwnership, unsupportedGeneralLicensing},
	}, nil
}

// Advise produces the recommendation.
func (s *Service) Advise(ctx context.Context, intake Intake) (Recommendation, error) {
	set, err := s.store.Published(ctx)
	if err != nil {
		return Recommendation{}, err
	}

	// Checked here rather than in the DTO because it depends on the rule set.
	// Rejected rather than ignored: an unknown activity would silently drop
	// every licence scoped to it, and the reader would read the resulting empty
	// licence list as "none needed".
	if intake.ActivityCode != "" {
		if _, ok := set.ActivityByCode(intake.ActivityCode); !ok {
			return Recommendation{}, fmt.Errorf("%w: %q", ErrUnknownActivity, intake.ActivityCode)
		}
	}

	rec := Recommendation{
		RuleSetID:    set.ID,
		RuleSetLabel: set.Label,
		Alternatives: []Candidate{},
		Excluded:     []Candidate{},
		Steps:        []Requirement{},
		Licences:     []Requirement{},
		Caveats:      []Requirement{},
	}

	candidates := Evaluate(intake, set)
	for _, candidate := range candidates {
		switch {
		case !candidate.Eligible:
			rec.Excluded = append(rec.Excluded, candidate)
		case rec.Recommended == nil:
			chosen := candidate
			rec.Recommended = &chosen
		default:
			rec.Alternatives = append(rec.Alternatives, candidate)
		}
	}

	// Requirements follow from the chosen form. With nothing eligible there is
	// no form to follow from, so only the form-independent requirements apply —
	// which is the honest answer to "every structure is ruled out": here is what
	// would have been true regardless, and here is why none of them work.
	structureCode := ""
	if rec.Recommended != nil {
		structureCode = rec.Recommended.Structure.Code
	}
	rec.Steps, rec.Licences, rec.Caveats =
		Requirements(set, structureCode, intake.ActivityCode)

	rec.Unsupported = unsupportedFor(intake, set)

	return rec, nil
}

// unsupportedFor names the gaps this intake actually ran into.
//
// Scoped to what the reader asked, not a blanket disclaimer: a sole trader who
// said nothing about foreign ownership does not need to be told the investment
// proclamation is missing, and burying the real limitation in boilerplate is how
// it stops being read.
func unsupportedFor(intake Intake, set RuleSet) []string {
	var out []string

	if intake.ForeignOwnership != nil && *intake.ForeignOwnership {
		out = append(out, unsupportedForeignOwnership)
	}

	// Licensing is flagged whenever an activity was named, because that is the
	// point at which the reader is entitled to expect a licence list and the
	// corpus cannot produce a complete one.
	if intake.ActivityCode != "" {
		if activity, ok := set.ActivityByCode(intake.ActivityCode); ok && !activity.Regulated {
			out = append(out, unsupportedGeneralLicensing)
		}
	}

	return out
}
