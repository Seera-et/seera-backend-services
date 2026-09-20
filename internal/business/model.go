// Package business is the Business & License Advisor: it turns a description of
// a planned business into a recommended legal form, the registration steps that
// follow from it, the licences the activity may need, and the provisions all of
// that rests on.
//
// The division of labour is the point. Every threshold — how many members a
// private limited company may have, what capital it needs — is *data*, loaded
// from the corpus-verified rule set in the database (see migration
// 0007_business_rules). Every decision is *code*, in rules.go, deterministic and
// containing no legal number of its own.
//
// Neither half involves a model. A recommendation is reproducible, explainable
// by the rule that produced it, and citable to the article behind that rule;
// asking an LLM to restate a capital requirement can only make it less so. See
// Recommendation for what that means for the SRS's "LLM Explanation" box.
package business

import (
	"fmt"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Liability is how members are exposed to the organisation's debts.
//
// Free text rather than an enum because the distinctions the Commercial Code
// draws do not reduce to two or three cases, and flattening them would lose the
// part a founder actually needs.
type Liability = string

// SourceRef is one provision a rule rests on.
//
// It names the document and article rather than carrying a chunk id, because a
// rule is authored against the law, not against a particular ingestion of it.
// Resolving to citable chunks happens per request, against whichever version is
// published now — so a re-ingested document does not orphan the rule set.
type SourceRef struct {
	DocumentID    legal.DocumentID `json:"document_id"`
	DocumentTitle string           `json:"document_title"`
	ArticleNo     string           `json:"article_no"`

	// Pinpoint names the sub-article where it is worth naming ("495/4").
	Pinpoint string `json:"pinpoint,omitempty"`

	// Facet is which part of the rule this citation supports — the member
	// range, the capital minimum, the liability position — or "general" for one
	// that identifies the form itself. A finding cites the provision behind
	// that finding, never the structure's whole bibliography: a capital reason
	// citing the article that merely lists the forms is a reference that does
	// not support its sentence.
	Facet string `json:"facet,omitempty"`

	// ChunkID and Text are filled once the reference is resolved against the
	// published corpus. Empty means the article could not be resolved — which
	// the response reports rather than hides, because an uncitable rule is a
	// rule the reader cannot check.
	ChunkID      legal.ChunkID `json:"chunk_id,omitempty"`
	ArticleTitle string        `json:"article_title,omitempty"`
	Text         string        `json:"text,omitempty"`
}

// Cite renders the reference the way a lawyer would write it.
func (s SourceRef) Cite() string {
	article := "Article " + s.ArticleNo
	if s.Pinpoint != "" {
		article = "Article " + s.Pinpoint
	}
	if s.DocumentTitle == "" {
		return article
	}
	return s.DocumentTitle + ", " + article
}

// Facets a citation may support. See SourceRef.Facet.
const (
	FacetGeneral      = "general"
	FacetMembers      = "members"
	FacetCapital      = "capital"
	FacetLiability    = "liability"
	FacetPublicShares = "public_shares"
)

// sourcesFor picks the citations backing one facet of a rule.
//
// Falls back to the general ones — the provisions identifying the form — when
// the author tagged nothing more specific. It never falls back to *all*
// sources: that is what produced citations unrelated to the claim beside them.
func sourcesFor(refs []SourceRef, facet string) []SourceRef {
	var matched, general []SourceRef
	for _, ref := range refs {
		switch ref.Facet {
		case facet:
			matched = append(matched, ref)
		case FacetGeneral, "":
			general = append(general, ref)
		}
	}
	if len(matched) > 0 {
		return matched
	}
	return general
}

// Structure is one legal form a business may take.
//
// Every bound is a pointer, and nil means "not established from the indexed
// corpus" — which is emphatically not "no limit". Ethiopia's share company has
// a statutory minimum number of shareholders (Commercial Code Art. 473/1/a
// refers to it) but the indexed text never states the number, so MinMembers is
// nil for it and the engine must not pretend otherwise.
type Structure struct {
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	NameSuffix string    `json:"name_suffix,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Liability  Liability `json:"liability,omitempty"`

	MinMembers *int `json:"min_members,omitempty"`
	MaxMembers *int `json:"max_members,omitempty"`

	MinCapital   *float64 `json:"min_capital,omitempty"`
	MinSharePar  *float64 `json:"min_share_par,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	PublicShares *bool    `json:"allows_public_subscription,omitempty"`

	Ordinal int         `json:"-"`
	Sources []SourceRef `json:"sources,omitempty"`
}

// Activity is one kind of trade, from the Commercial Code's own enumeration of
// who counts as a trader.
type Activity struct {
	Code      string      `json:"code"`
	Name      string      `json:"name"`
	Regulated bool        `json:"regulated"`
	Ordinal   int         `json:"-"`
	Sources   []SourceRef `json:"sources,omitempty"`
}

// RequirementKind separates what a reader must do from what they must obtain
// and what they should merely know.
type RequirementKind string

const (
	// RequirementStep is an action: register, deposit, publish.
	RequirementStep RequirementKind = "step"
	// RequirementLicence is a permission the activity needs.
	RequirementLicence RequirementKind = "licence"
	// RequirementCaveat is something true and relevant that is not an
	// instruction — including, importantly, the limits of what the indexed
	// corpus can support.
	RequirementCaveat RequirementKind = "caveat"
)

// Requirement is one statement the advisor makes, with its source.
//
// StructureCode empty means it applies whatever form is chosen; ActivityCode
// empty means it applies whatever the business does.
type Requirement struct {
	Kind          RequirementKind `json:"kind"`
	StructureCode string          `json:"structure_code,omitempty"`
	ActivityCode  string          `json:"activity_code,omitempty"`

	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Authority string `json:"authority,omitempty"`

	Ordinal int         `json:"-"`
	Sources []SourceRef `json:"sources,omitempty"`
}

// RuleSet is one published, corpus-verified body of advisor rules.
type RuleSet struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Notes string `json:"notes,omitempty"`

	Structures   []Structure   `json:"structures"`
	Activities   []Activity    `json:"activities"`
	Requirements []Requirement `json:"requirements"`
}

// StructureByCode finds a form by its code.
func (r RuleSet) StructureByCode(code string) (Structure, bool) {
	for _, s := range r.Structures {
		if s.Code == code {
			return s, true
		}
	}
	return Structure{}, false
}

// ActivityByCode finds an activity by its code.
func (r RuleSet) ActivityByCode(code string) (Activity, bool) {
	for _, a := range r.Activities {
		if a.Code == code {
			return a, true
		}
	}
	return Activity{}, false
}

// Intake is what the reader told us about the business they plan.
//
// Deliberately small. Every field here has to be something a rule in the set
// actually turns on: asking a question whose answer changes nothing is a worse
// product than not asking it.
type Intake struct {
	// Founders is how many people will own the business. The single most
	// discriminating answer — it eliminates most forms on its own.
	Founders int `json:"founders"`

	// ActivityCode is one of the rule set's activities, or empty for "not sure
	// yet", which is a legitimate answer and must not block a recommendation.
	ActivityCode string `json:"activity_code,omitempty"`

	// CapitalBirr is what the founders can contribute, or nil for "unknown".
	// Nil never excludes a form; it produces a caveat instead.
	CapitalBirr *float64 `json:"capital_birr,omitempty"`

	// WantsLimitedLiability records a preference, not a legal constraint, so it
	// ranks candidates rather than eliminating them.
	WantsLimitedLiability *bool `json:"wants_limited_liability,omitempty"`

	// RaiseFromPublic is whether shares will be offered publicly. This one *is*
	// a legal constraint: a private limited company's shares may not be
	// (Commercial Code Art. 495/2).
	RaiseFromPublic *bool `json:"raise_from_public,omitempty"`

	// ForeignOwnership is carried but cannot be advised on: no investment
	// legislation is indexed. It exists so the answer can say so explicitly
	// rather than quietly ignoring the most consequential thing a foreign
	// founder asked about.
	ForeignOwnership *bool `json:"foreign_ownership,omitempty"`

	// Region is free text, unused by any rule today, and kept out of the DTO
	// for exactly that reason. See dto.go.
	Region string `json:"region,omitempty"`
}

// ReasonCode identifies why a rule reached its conclusion.
//
// A code rather than a sentence so the eval can assert on the reasoning and not
// on prose, and so the UI can style a disqualification differently from a
// preference.
type ReasonCode string

const (
	ReasonMembersBelowMinimum ReasonCode = "members_below_minimum"
	ReasonMembersAboveMaximum ReasonCode = "members_above_maximum"
	ReasonMembersWithinRange  ReasonCode = "members_within_range"
	ReasonMemberLimitUnknown  ReasonCode = "member_limit_unknown"

	ReasonCapitalBelowMinimum   ReasonCode = "capital_below_minimum"
	ReasonCapitalSufficient     ReasonCode = "capital_sufficient"
	ReasonCapitalUnknown        ReasonCode = "capital_unknown"
	ReasonCapitalNotEstablished ReasonCode = "capital_not_established"

	ReasonPublicSharesNotAllowed ReasonCode = "public_shares_not_allowed"
	ReasonPublicSharesAllowed    ReasonCode = "public_shares_allowed"

	ReasonLimitedLiabilityMatch    ReasonCode = "limited_liability_match"
	ReasonLimitedLiabilityMismatch ReasonCode = "limited_liability_mismatch"
)

// Reason is one finding about one structure, with the provisions behind it.
type Reason struct {
	Code ReasonCode `json:"code"`

	// Message states the finding in the reader's terms. Built by formatting the
	// rule data — never a number typed into Go source.
	Message string `json:"message"`

	// Disqualifying marks a finding that rules the structure out entirely, as
	// against one that only makes it a worse fit.
	Disqualifying bool `json:"disqualifying"`

	Sources []SourceRef `json:"sources,omitempty"`
}

// Candidate is one structure assessed against the intake.
type Candidate struct {
	Structure Structure `json:"structure"`
	Eligible  bool      `json:"eligible"`

	// Score ranks eligible candidates against each other. It is an ordering
	// device with no meaning outside one response, and is not shown as a number.
	Score int `json:"-"`

	Reasons []Reason `json:"reasons"`
}

// Recommendation is the advisor's full answer.
//
// There is no generated prose in here. Every sentence a reader sees is either a
// Reason built from the rule that fired, or the verbatim text of a provision
// resolved from the corpus. The SRS's Module C flow has an "LLM Explanation"
// stage; it is deliberately not implemented, because on this path a model could
// only paraphrase thresholds it must not restate, and a recommendation that
// cannot be traced to the rule that produced it fails AC-003's premise.
type Recommendation struct {
	RuleSetID    string `json:"rule_set_id"`
	RuleSetLabel string `json:"rule_set_label"`

	// Recommended is the best-fitting eligible form, or nil when the intake
	// eliminates every one of them — which is an answer, not an error.
	Recommended *Candidate `json:"recommended"`

	// Alternatives are the other eligible forms, best first.
	Alternatives []Candidate `json:"alternatives"`

	// Excluded are the forms the intake ruled out, each carrying the reason.
	// Returned because "why not a private limited company?" is the question a
	// founder asks next, and answering it is cheap.
	Excluded []Candidate `json:"excluded"`

	Steps    []Requirement `json:"steps"`
	Licences []Requirement `json:"licences"`
	Caveats  []Requirement `json:"caveats"`

	// Unsupported names the things the reader asked about that this corpus
	// cannot answer. Never empty when the intake raised one of them.
	Unsupported []string `json:"unsupported,omitempty"`
}

// formatMoney renders an amount the way the rule data carries it.
//
// Trailing zeros are dropped because the law says "fifteen thousand", not
// "15000.00", and a recommendation that reads like a spreadsheet invites the
// reader to check the wrong thing.
func formatMoney(amount float64, currency string) string {
	text := strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", amount), "0"), ".")
	if currency == "" {
		return text
	}
	return text + " " + currency
}
