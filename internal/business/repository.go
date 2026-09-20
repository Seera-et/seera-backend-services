package business

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// ErrNoRuleSet means nothing is published, so the advisor has no rules to
// apply.
//
// A distinct error rather than an empty rule set: "no legal form matches your
// answers" and "this deployment has no advisor rules loaded" are completely
// different statements, and conflating them would tell a founder their business
// is impossible when the truth is that an administrator has not finished
// setting up.
var ErrNoRuleSet = errors.New("no published business rule set")

// ErrUnknownArticle means a rule cited a provision the corpus does not have.
var ErrUnknownArticle = errors.New("cited article is not in the published corpus")

// Repository reads and writes rule sets.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Published loads the live rule set, with every citation resolved against the
// corpus as it stands now.
//
// One query per entity kind rather than one join: the shapes are different
// (structures have bounds, requirements have scopes) and a single join would
// multiply every row by its citation count and need de-duplicating in Go, which
// is more code than four small queries and easier to get wrong.
func (r *Repository) Published(ctx context.Context) (RuleSet, error) {
	const setQuery = `
		SELECT id::text, label, notes
		FROM business_rule_sets
		WHERE status = 'published'
		LIMIT 1`

	var set RuleSet
	if err := r.pool.QueryRow(ctx, setQuery).Scan(&set.ID, &set.Label, &set.Notes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RuleSet{}, ErrNoRuleSet
		}
		return RuleSet{}, fmt.Errorf("load rule set: %w", err)
	}

	sources, err := r.sources(ctx, set.ID)
	if err != nil {
		return RuleSet{}, err
	}

	if set.Structures, err = r.structures(ctx, set.ID, sources); err != nil {
		return RuleSet{}, err
	}
	if set.Activities, err = r.activities(ctx, set.ID, sources); err != nil {
		return RuleSet{}, err
	}
	if set.Requirements, err = r.requirements(ctx, set.ID, sources); err != nil {
		return RuleSet{}, err
	}

	return set, nil
}

// sourceKey identifies which subject a citation belongs to.
type sourceKey struct {
	kind string
	id   string
}

// sources loads every citation in the set, resolved to the article text.
//
// The join to articles goes through the *published* version, which is why a
// rule set cites (document, article number) rather than an article id: the rule
// outlives any particular ingestion of the document it cites. A LEFT JOIN, so a
// citation whose article has vanished from the current version still comes back
// — with empty text, which the response surfaces rather than dropping. A
// silently missing citation is how a rule stops being checkable.
func (r *Repository) sources(ctx context.Context, setID string) (map[sourceKey][]SourceRef, error) {
	const query = `
		SELECT
			s.subject_kind, s.subject_id::text,
			s.document_id::text, d.title, s.article_no, s.pinpoint, s.facet,
			a.title,
			(SELECT c.id::text FROM chunks c WHERE c.article_id = a.id ORDER BY c.ordinal LIMIT 1),
			a.text
		FROM business_rule_sources s
		JOIN legal_documents d ON d.id = s.document_id
		LEFT JOIN document_versions v
			ON v.document_id = s.document_id AND v.status = 'published'
		LEFT JOIN articles a
			ON a.version_id = v.id AND a.article_no = s.article_no
		WHERE s.rule_set_id = $1::uuid
		ORDER BY s.article_no`

	rows, err := r.pool.Query(ctx, query, setID)
	if err != nil {
		return nil, fmt.Errorf("load rule sources: %w", err)
	}
	defer rows.Close()

	out := map[sourceKey][]SourceRef{}
	for rows.Next() {
		var (
			key                             sourceKey
			ref                             SourceRef
			articleTitle, chunkID, textBody *string
		)
		if err := rows.Scan(
			&key.kind, &key.id,
			&ref.DocumentID, &ref.DocumentTitle, &ref.ArticleNo, &ref.Pinpoint, &ref.Facet,
			&articleTitle, &chunkID, &textBody,
		); err != nil {
			return nil, fmt.Errorf("scan rule source: %w", err)
		}
		if articleTitle != nil {
			ref.ArticleTitle = *articleTitle
		}
		if chunkID != nil {
			ref.ChunkID = legal.ChunkID(*chunkID)
		}
		if textBody != nil {
			ref.Text = *textBody
		}
		out[key] = append(out[key], ref)
	}
	return out, rows.Err()
}

func (r *Repository) structures(
	ctx context.Context, setID string, sources map[sourceKey][]SourceRef,
) ([]Structure, error) {
	const query = `
		SELECT id::text, code, name, name_suffix, summary, liability,
		       min_members, max_members,
		       min_capital, min_share_par, currency, allows_public_subscription,
		       ordinal
		FROM business_structures
		WHERE rule_set_id = $1::uuid
		ORDER BY ordinal`

	rows, err := r.pool.Query(ctx, query, setID)
	if err != nil {
		return nil, fmt.Errorf("load structures: %w", err)
	}
	defer rows.Close()

	out := []Structure{}
	for rows.Next() {
		var (
			id string
			s  Structure
		)
		if err := rows.Scan(
			&id, &s.Code, &s.Name, &s.NameSuffix, &s.Summary, &s.Liability,
			&s.MinMembers, &s.MaxMembers,
			&s.MinCapital, &s.MinSharePar, &s.Currency, &s.PublicShares,
			&s.Ordinal,
		); err != nil {
			return nil, fmt.Errorf("scan structure: %w", err)
		}
		s.Sources = sources[sourceKey{kind: "structure", id: id}]
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) activities(
	ctx context.Context, setID string, sources map[sourceKey][]SourceRef,
) ([]Activity, error) {
	const query = `
		SELECT id::text, code, name, regulated, ordinal
		FROM business_activities
		WHERE rule_set_id = $1::uuid
		ORDER BY ordinal`

	rows, err := r.pool.Query(ctx, query, setID)
	if err != nil {
		return nil, fmt.Errorf("load activities: %w", err)
	}
	defer rows.Close()

	out := []Activity{}
	for rows.Next() {
		var (
			id string
			a  Activity
		)
		if err := rows.Scan(&id, &a.Code, &a.Name, &a.Regulated, &a.Ordinal); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		a.Sources = sources[sourceKey{kind: "activity", id: id}]
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) requirements(
	ctx context.Context, setID string, sources map[sourceKey][]SourceRef,
) ([]Requirement, error) {
	const query = `
		SELECT id::text, kind,
		       coalesce(structure_code, ''), coalesce(activity_code, ''),
		       title, detail, authority, ordinal
		FROM business_requirements
		WHERE rule_set_id = $1::uuid
		ORDER BY kind, ordinal`

	rows, err := r.pool.Query(ctx, query, setID)
	if err != nil {
		return nil, fmt.Errorf("load requirements: %w", err)
	}
	defer rows.Close()

	out := []Requirement{}
	for rows.Next() {
		var (
			id string
			q  Requirement
		)
		if err := rows.Scan(
			&id, &q.Kind, &q.StructureCode, &q.ActivityCode,
			&q.Title, &q.Detail, &q.Authority, &q.Ordinal,
		); err != nil {
			return nil, fmt.Errorf("scan requirement: %w", err)
		}
		q.Sources = sources[sourceKey{kind: "requirement", id: id}]
		out = append(out, q)
	}
	return out, rows.Err()
}

// ErrUnknownActivity means the intake named an activity the rule set does not
// define.
var ErrUnknownActivity = errors.New("unknown activity")
