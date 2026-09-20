package business

import (
	"fmt"
	"strings"
)

// Bounds on the intake. Every one of these is a sanity limit on user input, not
// a legal rule — the law's own limits live in the rule set.
const (
	// A business organisation with more founders than this is not something a
	// questionnaire can usefully advise on, and the number is large enough that
	// no real intake reaches it.
	maxFounders = 10_000

	// Capital is bounded only to reject nonsense: a negative contribution, or a
	// figure so large it is certainly a typo or an overflow probe.
	maxCapital = 1e15
)

// AdviseRequest is the body of POST /api/v1/business/advisor.
//
// Pointers distinguish "not answered" from "answered no", which matters: a
// founder who has not said whether they want limited liability is in a
// different position from one who said they do not, and flattening the two
// would invent an answer on their behalf.
type AdviseRequest struct {
	Founders int `json:"founders"`

	ActivityCode string `json:"activity_code,omitempty"`

	CapitalBirr           *float64 `json:"capital_birr,omitempty"`
	WantsLimitedLiability *bool    `json:"wants_limited_liability,omitempty"`
	RaiseFromPublic       *bool    `json:"raise_from_public,omitempty"`
	ForeignOwnership      *bool    `json:"foreign_ownership,omitempty"`
}

// Validate bounds the request at the edge.
//
// Note what is *not* validated here: whether ActivityCode names a real activity.
// That depends on the published rule set, so it is checked in the service where
// the set is in hand — validating it here would need the DTO layer to reach into
// the database.
func (r AdviseRequest) Validate() error {
	switch {
	case r.Founders < 1:
		return fmt.Errorf("founders must be at least 1, got %d", r.Founders)
	case r.Founders > maxFounders:
		return fmt.Errorf("founders must be at most %d, got %d", maxFounders, r.Founders)
	}

	if r.CapitalBirr != nil {
		switch {
		case *r.CapitalBirr < 0:
			return fmt.Errorf("capital_birr cannot be negative")
		case *r.CapitalBirr > maxCapital:
			return fmt.Errorf("capital_birr is implausibly large")
		}
	}

	if len(r.ActivityCode) > 64 {
		return fmt.Errorf("activity_code is too long")
	}
	return nil
}

// Intake converts the request into the engine's input.
func (r AdviseRequest) Intake() Intake {
	return Intake{
		Founders:              r.Founders,
		ActivityCode:          strings.TrimSpace(r.ActivityCode),
		CapitalBirr:           r.CapitalBirr,
		WantsLimitedLiability: r.WantsLimitedLiability,
		RaiseFromPublic:       r.RaiseFromPublic,
		ForeignOwnership:      r.ForeignOwnership,
	}
}

// IntakeForm is the questionnaire, served by GET /api/v1/business/intake.
//
// The options come from the published rule set rather than from the frontend,
// for the same reason the thresholds do: "the legal forms available in Ethiopia"
// and "what counts as a trade" are statements about Ethiopian law, and a list of
// them hardcoded in TypeScript is a legal claim in application code with no
// source attached.
type IntakeForm struct {
	RuleSetID    string `json:"rule_set_id"`
	RuleSetLabel string `json:"rule_set_label"`

	Activities []Activity  `json:"activities"`
	Structures []Structure `json:"structures"`

	// Unsupported names what this deployment's corpus cannot advise on, so the
	// UI can say so beside the question rather than after the reader has
	// answered it.
	Unsupported []string `json:"unsupported,omitempty"`
}
