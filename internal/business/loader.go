package business

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// RuleSetInput is a rule set as authored, before it has been checked against
// the corpus.
//
// Citations name a document by a substring of its title rather than by id: a
// curated file cannot know the uuids of a particular database, and a title is
// what the author was reading when they wrote the rule.
type RuleSetInput struct {
	Label string `json:"label"`
	Notes string `json:"notes"`

	Structures   []StructureInput   `json:"structures"`
	Activities   []ActivityInput    `json:"activities"`
	Requirements []RequirementInput `json:"requirements"`
}

// SourceInput is one citation as authored.
type SourceInput struct {
	Document string `json:"document"`
	Article  string `json:"article"`
	Pinpoint string `json:"pinpoint,omitempty"`

	// Facet is which part of the rule this citation supports. Empty means
	// "general" — it identifies the form rather than backing one of its bounds.
	// Tag it wherever a specific bound has a specific article: an untagged
	// capital citation makes the capital reason cite whatever else is listed.
	Facet string `json:"facet,omitempty"`
}

type StructureInput struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	NameSuffix string `json:"name_suffix,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Liability  string `json:"liability,omitempty"`

	// Omit a bound to say the corpus does not establish it. Do not write a
	// large number to mean "no limit" — the engine distinguishes the two and
	// tells the reader which it is.
	MinMembers *int `json:"min_members,omitempty"`
	MaxMembers *int `json:"max_members,omitempty"`

	MinCapital   *float64 `json:"min_capital,omitempty"`
	MinSharePar  *float64 `json:"min_share_par,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	PublicShares *bool    `json:"allows_public_subscription,omitempty"`

	Sources []SourceInput `json:"sources"`
}

type ActivityInput struct {
	Code      string        `json:"code"`
	Name      string        `json:"name"`
	Regulated bool          `json:"regulated,omitempty"`
	Sources   []SourceInput `json:"sources"`
}

type RequirementInput struct {
	Kind string `json:"kind"`

	// StructureCode scopes the requirement to one form; StructureCodes scopes
	// it to several, expanding to one stored row each.
	//
	// There is deliberately no "applies to all except" form. The Commercial
	// Code's exceptions are specific — a joint venture is not registered
	// (Art. 175), a sole trader has no memorandum (Art. 172) — and listing the
	// forms a rule does apply to forces the author to decide about each one,
	// where a negation quietly sweeps in every form added later.
	StructureCode  string   `json:"structure_code,omitempty"`
	StructureCodes []string `json:"structure_codes,omitempty"`

	ActivityCode string `json:"activity_code,omitempty"`

	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Authority string `json:"authority,omitempty"`

	Sources []SourceInput `json:"sources"`
}

// Validate checks the set is internally coherent, before any database work.
//
// Structural problems are caught here so the transaction below only ever has to
// deal with the one thing it alone can check: whether the cited law exists.
func (in RuleSetInput) Validate() error {
	if strings.TrimSpace(in.Label) == "" {
		return errors.New("label is required")
	}
	if len(in.Structures) == 0 {
		return errors.New("a rule set with no structures cannot recommend anything")
	}

	structures := map[string]bool{}
	for i, s := range in.Structures {
		switch {
		case strings.TrimSpace(s.Code) == "":
			return fmt.Errorf("structure %d has no code", i)
		case strings.TrimSpace(s.Name) == "":
			return fmt.Errorf("structure %q has no name", s.Code)
		case structures[s.Code]:
			return fmt.Errorf("duplicate structure code %q", s.Code)
		case len(s.Sources) == 0:
			// The whole design rests on every threshold being traceable. A
			// structure with no citation is a legal claim with no source, which
			// is the one thing this package exists to prevent.
			return fmt.Errorf("structure %q cites nothing", s.Code)
		case s.MinMembers != nil && *s.MinMembers < 1:
			return fmt.Errorf("structure %q has a minimum of %d members", s.Code, *s.MinMembers)
		case s.MinMembers != nil && s.MaxMembers != nil && *s.MinMembers > *s.MaxMembers:
			return fmt.Errorf("structure %q has a minimum above its maximum", s.Code)
		case s.MinCapital != nil && *s.MinCapital < 0:
			return fmt.Errorf("structure %q has negative capital", s.Code)
		case s.MinCapital != nil && strings.TrimSpace(s.Currency) == "":
			return fmt.Errorf("structure %q states capital without a currency", s.Code)
		}
		structures[s.Code] = true

		if err := validateSources(s.Sources, "structure "+s.Code); err != nil {
			return err
		}
	}

	activities := map[string]bool{}
	for i, a := range in.Activities {
		switch {
		case strings.TrimSpace(a.Code) == "":
			return fmt.Errorf("activity %d has no code", i)
		case strings.TrimSpace(a.Name) == "":
			return fmt.Errorf("activity %q has no name", a.Code)
		case activities[a.Code]:
			return fmt.Errorf("duplicate activity code %q", a.Code)
		}
		activities[a.Code] = true

		if err := validateSources(a.Sources, "activity "+a.Code); err != nil {
			return err
		}
	}

	for i, r := range in.Requirements {
		switch {
		case strings.TrimSpace(r.Title) == "":
			return fmt.Errorf("requirement %d has no title", i)
		case !validRequirementKind(r.Kind):
			return fmt.Errorf("requirement %q has kind %q, want step, licence or caveat",
				r.Title, r.Kind)
		// A scope naming something the set does not contain is a rule that can
		// never fire — silently dead rather than wrong, which is worse because
		// nothing ever reports it.
		case r.StructureCode != "" && !structures[r.StructureCode]:
			return fmt.Errorf("requirement %q is scoped to unknown structure %q",
				r.Title, r.StructureCode)
		case r.StructureCode != "" && len(r.StructureCodes) > 0:
			return fmt.Errorf("requirement %q sets both structure_code and structure_codes",
				r.Title)
		case r.ActivityCode != "" && !activities[r.ActivityCode]:
			return fmt.Errorf("requirement %q is scoped to unknown activity %q",
				r.Title, r.ActivityCode)
		}

		// Caveats may be uncited: "no investment legislation is indexed" is a
		// true and necessary statement about the corpus with no article behind
		// it. Steps and licences are legal obligations and must cite one.
		if r.Kind != string(RequirementCaveat) && len(r.Sources) == 0 {
			return fmt.Errorf("%s %q cites nothing", r.Kind, r.Title)
		}
		for _, code := range r.StructureCodes {
			if !structures[code] {
				return fmt.Errorf("requirement %q is scoped to unknown structure %q",
					r.Title, code)
			}
		}
		if err := validateSources(r.Sources, r.Kind+" "+r.Title); err != nil {
			return err
		}
	}

	return nil
}

func validateSources(sources []SourceInput, subject string) error {
	for _, s := range sources {
		if strings.TrimSpace(s.Document) == "" {
			return fmt.Errorf("%s has a citation with no document", subject)
		}
		if strings.TrimSpace(s.Article) == "" {
			return fmt.Errorf("%s has a citation with no article number", subject)
		}
		if !validFacet(s.Facet) {
			return fmt.Errorf("%s cites Article %s with unknown facet %q",
				subject, s.Article, s.Facet)
		}
	}
	return nil
}

func validFacet(facet string) bool {
	switch facet {
	case "", FacetGeneral, FacetMembers, FacetCapital, FacetLiability, FacetPublicShares:
		return true
	default:
		return false
	}
}

func validRequirementKind(kind string) bool {
	switch RequirementKind(kind) {
	case RequirementStep, RequirementLicence, RequirementCaveat:
		return true
	default:
		return false
	}
}

// Publish stores a rule set and makes it live, in one transaction.
//
// Every citation is resolved against the published corpus first, and an
// unresolvable one fails the whole publish. That is the load-bearing check in
// this package: it makes it impossible to have a live rule asserting a capital
// requirement whose article is not in the corpus, so "the rules are cited" is a
// property of the system rather than a promise about the curation process.
//
// Publishing last, after every insert, mirrors document ingestion: a failed
// load leaves nothing queryable.
func (r *Repository) Publish(ctx context.Context, in RuleSetInput) (string, error) {
	if err := in.Validate(); err != nil {
		return "", fmt.Errorf("invalid rule set: %w", err)
	}

	// Resolution reads the corpus and does not write, so it happens before the
	// transaction opens rather than holding one across every lookup.
	documents, err := r.resolveDocuments(ctx, in)
	if err != nil {
		return "", err
	}
	if err := r.verifyArticles(ctx, in, documents); err != nil {
		return "", err
	}

	var setID string
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Retire the current set first: the partial unique index permits only
		// one published row, so the new one cannot be inserted as published
		// while the old one still is.
		if _, err := tx.Exec(ctx,
			`UPDATE business_rule_sets SET status = 'superseded' WHERE status = 'published'`,
		); err != nil {
			return fmt.Errorf("supersede current rule set: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO business_rule_sets (label, notes, status)
			VALUES ($1, $2, 'draft')
			ON CONFLICT (label) DO UPDATE SET notes = excluded.notes, status = 'draft'
			RETURNING id::text`, in.Label, in.Notes).Scan(&setID); err != nil {
			return fmt.Errorf("insert rule set: %w", err)
		}

		// A re-published label replaces its previous contents rather than
		// accumulating them.
		for _, table := range []string{
			"business_rule_sources", "business_requirements",
			"business_activities", "business_structures",
		} {
			if _, err := tx.Exec(ctx,
				`DELETE FROM `+table+` WHERE rule_set_id = $1::uuid`, setID); err != nil {
				return fmt.Errorf("clear %s: %w", table, err)
			}
		}

		if err := insertStructures(ctx, tx, setID, in, documents); err != nil {
			return err
		}
		if err := insertActivities(ctx, tx, setID, in, documents); err != nil {
			return err
		}
		if err := insertRequirements(ctx, tx, setID, in, documents); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE business_rule_sets
			SET status = 'published', published_at = now()
			WHERE id = $1::uuid`, setID); err != nil {
			return fmt.Errorf("publish rule set: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return setID, nil
}

// resolveDocuments maps every cited document title to exactly one document.
//
// Ambiguity is an error, not a guess. Two documents matching "Commercial Code"
// — the 1960 edition and the 2021 one — is precisely the case where picking one
// silently would attach a rule to the wrong law.
func (r *Repository) resolveDocuments(
	ctx context.Context, in RuleSetInput,
) (map[string]legal.DocumentID, error) {
	titles := map[string]bool{}
	for _, group := range allSources(in) {
		for _, s := range group {
			titles[s.Document] = true
		}
	}

	out := make(map[string]legal.DocumentID, len(titles))
	for title := range titles {
		const query = `
			SELECT d.id::text, d.title
			FROM legal_documents d
			JOIN document_versions v ON v.document_id = d.id AND v.status = 'published'
			WHERE d.title ILIKE '%' || $1 || '%'`

		rows, err := r.pool.Query(ctx, query, title)
		if err != nil {
			return nil, fmt.Errorf("resolve document %q: %w", title, err)
		}

		var (
			ids    []string
			titles []string
		)
		for rows.Next() {
			var id, matched string
			if err := rows.Scan(&id, &matched); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan document match: %w", err)
			}
			ids = append(ids, id)
			titles = append(titles, matched)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("resolve document %q: %w", title, err)
		}

		switch len(ids) {
		case 0:
			return nil, fmt.Errorf(
				"%w: no published document matches %q, so a rule citing it cannot be verified",
				ErrUnknownArticle, title)
		case 1:
			out[title] = legal.DocumentID(ids[0])
		default:
			return nil, fmt.Errorf(
				"citation %q matches %d published documents (%s); make it specific enough to name one",
				title, len(ids), strings.Join(titles, "; "))
		}
	}
	return out, nil
}

// verifyArticles checks every cited article exists in its document's published
// version.
func (r *Repository) verifyArticles(
	ctx context.Context, in RuleSetInput, documents map[string]legal.DocumentID,
) error {
	const query = `
		SELECT EXISTS (
			SELECT 1
			FROM articles a
			JOIN document_versions v ON v.id = a.version_id AND v.status = 'published'
			WHERE a.document_id = $1::uuid AND a.article_no = $2)`

	checked := map[string]bool{}
	for _, group := range allSources(in) {
		for _, s := range group {
			documentID := documents[s.Document]
			key := string(documentID) + "/" + s.Article
			if checked[key] {
				continue
			}
			checked[key] = true

			var exists bool
			if err := r.pool.QueryRow(ctx, query, string(documentID), s.Article).Scan(&exists); err != nil {
				return fmt.Errorf("verify article %s of %q: %w", s.Article, s.Document, err)
			}
			if !exists {
				return fmt.Errorf("%w: %q has no Article %s in its published version",
					ErrUnknownArticle, s.Document, s.Article)
			}
		}
	}
	return nil
}

// allSources gathers every citation group in the set, so resolution and
// verification cannot miss one by forgetting a field.
func allSources(in RuleSetInput) [][]SourceInput {
	groups := make([][]SourceInput, 0,
		len(in.Structures)+len(in.Activities)+len(in.Requirements))
	for _, s := range in.Structures {
		groups = append(groups, s.Sources)
	}
	for _, a := range in.Activities {
		groups = append(groups, a.Sources)
	}
	for _, r := range in.Requirements {
		groups = append(groups, r.Sources)
	}
	return groups
}

func insertStructures(
	ctx context.Context, tx pgx.Tx, setID string,
	in RuleSetInput, documents map[string]legal.DocumentID,
) error {
	for i, s := range in.Structures {
		currency := s.Currency
		if currency == "" {
			currency = "ETB"
		}

		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO business_structures (
				rule_set_id, code, name, name_suffix, summary, liability,
				min_members, max_members, min_capital, min_share_par, currency,
				allows_public_subscription, ordinal)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING id::text`,
			setID, s.Code, s.Name, s.NameSuffix, s.Summary, s.Liability,
			s.MinMembers, s.MaxMembers, s.MinCapital, s.MinSharePar, currency,
			s.PublicShares, i,
		).Scan(&id); err != nil {
			return fmt.Errorf("insert structure %q: %w", s.Code, err)
		}
		if err := insertSources(ctx, tx, setID, "structure", id, s.Sources, documents); err != nil {
			return err
		}
	}
	return nil
}

func insertActivities(
	ctx context.Context, tx pgx.Tx, setID string,
	in RuleSetInput, documents map[string]legal.DocumentID,
) error {
	for i, a := range in.Activities {
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO business_activities (rule_set_id, code, name, regulated, ordinal)
			VALUES ($1::uuid, $2, $3, $4, $5)
			RETURNING id::text`,
			setID, a.Code, a.Name, a.Regulated, i,
		).Scan(&id); err != nil {
			return fmt.Errorf("insert activity %q: %w", a.Code, err)
		}
		if err := insertSources(ctx, tx, setID, "activity", id, a.Sources, documents); err != nil {
			return err
		}
	}
	return nil
}

func insertRequirements(
	ctx context.Context, tx pgx.Tx, setID string,
	in RuleSetInput, documents map[string]legal.DocumentID,
) error {
	for i, q := range in.Requirements {
		// One row per scope. The ordinal stays the authored position so a
		// requirement expanded across six forms still sorts where its author
		// put it, rather than after everything written later.
		scopes := q.StructureCodes
		if len(scopes) == 0 {
			scopes = []string{q.StructureCode}
		}

		for _, scope := range scopes {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO business_requirements (
					rule_set_id, kind, structure_code, activity_code,
					title, detail, authority, ordinal)
				VALUES ($1::uuid, $2, nullif($3, ''), nullif($4, ''), $5, $6, $7, $8)
				RETURNING id::text`,
				setID, q.Kind, scope, q.ActivityCode,
				q.Title, q.Detail, q.Authority, i,
			).Scan(&id); err != nil {
				return fmt.Errorf("insert requirement %q: %w", q.Title, err)
			}
			if err := insertSources(ctx, tx, setID, "requirement", id, q.Sources, documents); err != nil {
				return err
			}
		}
	}
	return nil
}

func facetOrGeneral(facet string) string {
	if facet == "" {
		return FacetGeneral
	}
	return facet
}

func insertSources(
	ctx context.Context, tx pgx.Tx, setID, kind, subjectID string,
	sources []SourceInput, documents map[string]legal.DocumentID,
) error {
	for _, s := range sources {
		if _, err := tx.Exec(ctx, `
			INSERT INTO business_rule_sources (
				rule_set_id, subject_kind, subject_id, document_id, article_no, pinpoint, facet)
			VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6, $7)
			ON CONFLICT DO NOTHING`,
			setID, kind, subjectID, string(documents[s.Document]), s.Article, s.Pinpoint,
			facetOrGeneral(s.Facet),
		); err != nil {
			return fmt.Errorf("insert citation %s %s: %w", s.Document, s.Article, err)
		}
	}
	return nil
}
