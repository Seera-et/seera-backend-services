package documents

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

// Bounds on a search request. A reader pages through results; nobody needs the
// whole corpus in one response, and an unbounded limit is how you ask for it.
const (
	defaultSearchPageSize = 20
	maxSearchPageSize     = 50

	// perArm is how many candidates each retrieval arm contributes before
	// fusion. Larger than the page size on purpose: fusion reorders, so a
	// result that lands on page 1 may have been ranked 30th by the arm that
	// found it.
	perArm = 80

	// maxPool caps the fused candidate list. Deep paging through a relevance
	// ranking is not a thing readers do — past a couple of hundred hits the
	// ordering carries no information and the honest move is to say so and let
	// them narrow the query instead.
	maxPool = 240
)

// Highlight markers wrap the matched terms in a snippet.
//
// Unicode Private Use Area codepoints rather than HTML tags: the frontend
// splits on these and builds elements, so a snippet is never parsed as markup
// and there is no path from legal text to injected HTML. They are also
// guaranteed absent from any real document, which a sentinel like "**" is not.
const (
	HighlightStart = "\ue000"
	HighlightEnd   = "\ue001"
)

// SearchMode selects which retrieval arms rank the results.
//
// Exposed rather than fixed because the SRS asks for keyword search and
// semantic search as separate capabilities (FR-EXP-001, FR-EXP-002), and
// because they fail differently: keyword finds the term a lawyer typed,
// semantic finds the provision a citizen described. A reader who knows which
// one they want should be able to say so.
type SearchMode string

const (
	// SearchModeHybrid fuses every available arm. The default.
	SearchModeHybrid SearchMode = "hybrid"
	// SearchModeKeyword is lexical only: no embedding call, so it also works on
	// a deployment with no AI credentials.
	SearchModeKeyword SearchMode = "keyword"
	// SearchModeSemantic is embedding similarity only.
	SearchModeSemantic SearchMode = "semantic"
)

func validSearchMode(m SearchMode) bool {
	switch m {
	case SearchModeHybrid, SearchModeKeyword, SearchModeSemantic:
		return true
	default:
		return false
	}
}

// SearchFilter is a validated corpus search.
type SearchFilter struct {
	// Term is what the reader typed. Required — this endpoint ranks by
	// relevance, and there is no relevance without a query.
	Term string

	Mode     SearchMode
	Language legal.Language
	DocType  legal.DocType

	// Domain matches legal_documents.legal_domain exactly, as offered by the
	// facet list in Stats. Free text would be a filter that silently matches
	// nothing.
	Domain string

	// Year matches the document's publication or effective year. Either one,
	// because the corpus has documents that record only one of the two.
	Year int

	// DocumentID restricts the search to a single document, which is what
	// "search within this document" means.
	DocumentID legal.DocumentID

	// IncludeSuperseded widens the search to historical versions.
	//
	// Off by default: a reader searching the corpus means the law as it stands,
	// and quietly mixing repealed provisions into that would be the same
	// mistake in the Explorer that the answer path exists to prevent. Every hit
	// carries its version status either way, so when this is on the reader can
	// see which is which.
	IncludeSuperseded bool

	Limit  int
	Offset int
}

// ParseSearchFilter reads and bounds the query string.
//
// Unknown filter values are rejected rather than ignored, for the same reason
// as in ParseListFilter: a filter that silently does nothing looks to the
// reader like a corpus that is missing documents.
func ParseSearchFilter(q url.Values) (SearchFilter, error) {
	f := SearchFilter{Mode: SearchModeHybrid, Limit: defaultSearchPageSize}

	f.Term = strings.TrimSpace(q.Get("q"))
	if f.Term == "" {
		return SearchFilter{}, fmt.Errorf("q is required")
	}
	if len([]rune(f.Term)) > maxSearchTermRunes {
		return SearchFilter{}, fmt.Errorf("q is longer than %d characters", maxSearchTermRunes)
	}

	if raw := strings.TrimSpace(q.Get("mode")); raw != "" {
		mode := SearchMode(strings.ToLower(raw))
		if !validSearchMode(mode) {
			return SearchFilter{}, fmt.Errorf(
				"mode %q is not one of hybrid, keyword, semantic", raw)
		}
		f.Mode = mode
	}

	if raw := strings.TrimSpace(q.Get("language")); raw != "" {
		lang := legal.Language(raw)
		if !lang.Valid() {
			return SearchFilter{}, fmt.Errorf("language %q is not one of en, am", raw)
		}
		f.Language = lang
	}

	if raw := strings.TrimSpace(q.Get("type")); raw != "" {
		docType := legal.DocType(raw)
		if !validDocType(docType) {
			return SearchFilter{}, fmt.Errorf(
				"type %q is not one of proclamation, regulation, directive, code, other", raw)
		}
		f.DocType = docType
	}

	f.Domain = strings.TrimSpace(q.Get("domain"))
	if len([]rune(f.Domain)) > maxSearchTermRunes {
		return SearchFilter{}, fmt.Errorf("domain is longer than %d characters", maxSearchTermRunes)
	}

	// Bounded to dates a legal corpus can plausibly carry. The lower end is
	// deliberately generous: Ethiopian codes in force today were promulgated in
	// the 1950s and 60s.
	year, err := intParam(q, "year", 0, 1800, 2200)
	if err != nil {
		return SearchFilter{}, err
	}
	f.Year = year

	f.DocumentID = legal.DocumentID(strings.TrimSpace(q.Get("document")))

	if raw := strings.TrimSpace(q.Get("include_superseded")); raw != "" {
		include, err := strconv.ParseBool(raw)
		if err != nil {
			return SearchFilter{}, fmt.Errorf(
				"include_superseded must be true or false, got %q", raw)
		}
		f.IncludeSuperseded = include
	}

	limit, err := intParam(q, "limit", defaultSearchPageSize, 1, maxSearchPageSize)
	if err != nil {
		return SearchFilter{}, err
	}
	f.Limit = limit

	offset, err := intParam(q, "offset", 0, 0, maxPool)
	if err != nil {
		return SearchFilter{}, err
	}
	f.Offset = offset

	return f, nil
}

// SearchHit is one article matched by a search.
//
// It carries its own provenance — document, version, status, article — because
// a result list that needs a second request to say which law a hit belongs to
// is a result list a reader cannot trust at a glance.
type SearchHit struct {
	DocumentID       legal.DocumentID `json:"document_id"`
	DocumentTitle    string           `json:"document_title"`
	DocType          legal.DocType    `json:"doc_type"`
	IssuingAuthority string           `json:"issuing_authority,omitempty"`
	Language         legal.Language   `json:"language"`

	VersionID     legal.VersionID     `json:"version_id"`
	VersionLabel  string              `json:"version_label"`
	VersionStatus legal.VersionStatus `json:"version_status"`

	ArticleID    legal.ArticleID `json:"article_id"`
	ArticleNo    string          `json:"article_no"`
	ArticleTitle string          `json:"article_title,omitempty"`
	Chapter      string          `json:"chapter,omitempty"`
	SectionPath  []string        `json:"section_path,omitempty"`

	// Ordinal is the article's position in its version. Carried so a result can
	// link into the document viewer at the right page of articles rather than
	// making the reader page there from the top.
	Ordinal int `json:"ordinal"`

	// ChunkID is the article's first retrievable chunk — the same identifier a
	// citation uses, so a reader can pivot from a search hit into the answer
	// path without a second lookup.
	ChunkID legal.ChunkID `json:"chunk_id,omitempty"`

	// Snippet is the matched text with HighlightStart/HighlightEnd around the
	// terms. On a semantic-only hit there is no lexical match to mark, so it is
	// the head of the provision instead.
	Snippet string `json:"snippet"`

	StructureConfidence legal.StructureConfidence `json:"structure_confidence"`

	// Score is the fused rank score. Comparable within one response and
	// meaningless outside it — it is not a relevance percentage and is not
	// shown as one.
	Score float64 `json:"score"`

	// MatchedBy names the arms that found this hit, best-first. Agreement
	// between lexical and semantic retrieval is the strongest signal either one
	// produces, and it is the first thing worth knowing when a result looks
	// wrong.
	MatchedBy []string `json:"matched_by"`
}

// SearchResult is one page of ranked hits.
type SearchResult struct {
	Hits []SearchHit `json:"hits"`

	// Total is how many distinct articles were ranked — the pool this page is a
	// slice of, not a corpus-wide count of everything that would ever match.
	// With Capped it is an exact statement about what can be paged through.
	Total int `json:"total"`

	// Capped reports that at least one arm returned as many candidates as it
	// was allowed, so matches exist beyond the ranked pool. The UI says "240+"
	// rather than "240" when this is set, because claiming an exact total that
	// is really a truncation point would be a lie about the corpus.
	Capped bool `json:"capped"`

	// Mode is the mode actually used, which is not always the one requested:
	// a deployment with no embedding provider serves a hybrid request as
	// keyword-only rather than failing it. Reported so the UI can say so.
	Mode SearchMode `json:"mode"`

	// ArticleRefs are the article numbers recognised in the query, if any.
	// Surfaced so the UI can explain why Article 45 is sitting at the top.
	ArticleRefs []string `json:"article_refs,omitempty"`

	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Pool is the ranked candidate list a search produced, before any of it is
// hydrated for display.
type Pool struct {
	Ranked []ranked

	// Capped reports that an arm filled its quota, so the corpus holds matches
	// beyond the ones ranked here. See SearchResult.Capped.
	Capped bool

	// Mode is the mode actually used, which can be narrower than the one
	// requested when no embedding provider is configured.
	Mode SearchMode
}

// articleNumbersIn finds the article numbers a query is asking for.
//
// Two cases. The first is a reference inside a sentence — "what does article 45
// say about capital" — which search.ParseArticleRefs already handles for the
// answer path, Ge'ez numerals included.
//
// The second is specific to a search box: a query that is *only* a number. In a
// legal corpus "45" is not a phrase somebody is looking for, it is a provision,
// and treating it as free text returns everything that mentions the number 45
// while burying Article 45 itself.
func articleNumbersIn(term string) []string {
	refs := search.ParseArticleRefs(term)
	if len(refs) > 0 {
		numbers := make([]string, len(refs))
		for i, ref := range refs {
			numbers[i] = ref.Number
		}
		return numbers
	}

	// NormalizeNumber converts Ge'ez and otherwise returns the input unchanged,
	// so the digit check is what decides whether this was a number at all.
	if normalized := search.NormalizeNumber(term); isDigits(normalized) {
		return []string{normalized}
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// candidate is one arm's hit on an article, before fusion.
//
// Score is the arm's own number and is comparable only within that arm. It
// exists so the keyword arm can merge its two per-language result sets in the
// right order; fusion itself reads rank, never this.
type candidate struct {
	ArticleID legal.ArticleID
	ChunkID   legal.ChunkID
	Score     float64
}

// arm is one retrieval strategy's ranked output.
type arm struct {
	name string
	// hits is ordered best-first and holds at most one entry per article.
	hits []candidate
	// saturated is true when the arm returned every candidate it was allowed,
	// meaning the corpus holds more matches than were ranked.
	saturated bool
}

// ranked is a fused article, after RRF.
type ranked struct {
	articleID legal.ArticleID
	chunkID   legal.ChunkID
	score     float64
	matchedBy []string
}

// fuse merges the arms by Reciprocal Rank Fusion.
//
// Same algorithm and same constant as the answer path's search.FuseRRF, for the
// same reason: fusing by rank avoids normalising a cosine similarity against a
// ts_rank_cd score, two numbers on unrelated scales. It is a separate
// implementation because it fuses articles rather than chunks and reports every
// arm that agreed rather than collapsing them to one label — a reader is shown
// the provenance, where the generator only needed the ordering.
func fuse(arms []arm, limit int) []ranked {
	type entry struct {
		ranked
		seen map[string]bool
	}

	byArticle := make(map[legal.ArticleID]*entry)
	order := make([]legal.ArticleID, 0, 64) // first-seen order, for stable ties

	for _, a := range arms {
		for rank, hit := range a.hits {
			contribution := 1.0 / (search.RRFConstant + float64(rank+1))

			e, ok := byArticle[hit.ArticleID]
			if !ok {
				e = &entry{
					ranked: ranked{articleID: hit.ArticleID, chunkID: hit.ChunkID},
					seen:   map[string]bool{},
				}
				byArticle[hit.ArticleID] = e
				order = append(order, hit.ArticleID)
			}
			e.score += contribution
			if !e.seen[a.name] {
				e.seen[a.name] = true
				e.matchedBy = append(e.matchedBy, a.name)
			}
		}
	}

	out := make([]ranked, 0, len(order))
	for _, id := range order {
		out = append(out, byArticle[id].ranked)
	}

	// Stable sort over first-seen order, so two articles with identical scores
	// come back in a deterministic sequence rather than map order.
	sortRankedDesc(out)

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortRankedDesc(r []ranked) {
	sort.SliceStable(r, func(i, j int) bool { return r[i].score > r[j].score })
}
