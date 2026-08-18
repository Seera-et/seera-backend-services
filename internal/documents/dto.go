// Package documents is the read side of the corpus catalogue: what has been
// ingested, which versions exist, and the articles inside a version.
//
// It is a browse API, not a search API. Retrieval for answering lives in
// internal/search and is reached through internal/rag; nothing here ranks,
// embeds, or generates. The two share one rule: only published versions are
// visible, and that predicate lives in this package's repository so no handler
// can forget it.
package documents

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Bounds on what a caller may ask for. Page sizes are capped because an
// unbounded limit is a way to ask the database for the whole corpus in one
// request.
const (
	defaultPageSize = 20
	maxPageSize     = 100

	defaultArticlePageSize = 50
	maxArticlePageSize     = 200

	maxSearchTermRunes = 200
)

// VersionSummary is one version of a document, without its text.
type VersionSummary struct {
	ID            legal.VersionID     `json:"id"`
	Label         string              `json:"version_label"`
	Status        legal.VersionStatus `json:"status"`
	EffectiveFrom *time.Time          `json:"effective_from,omitempty"`
	RepealedAt    *time.Time          `json:"repealed_at,omitempty"`
	PublishedAt   *time.Time          `json:"published_at,omitempty"`
	ArticleCount  int                 `json:"article_count"`
}

// DocumentSummary is a catalogue entry: the document's identity plus its
// currently published version.
type DocumentSummary struct {
	ID               legal.DocumentID `json:"id"`
	Title            string           `json:"title"`
	DocType          legal.DocType    `json:"doc_type"`
	LegalDomain      string           `json:"legal_domain,omitempty"`
	IssuingAuthority string           `json:"issuing_authority,omitempty"`
	Language         legal.Language   `json:"language"`
	PublicationDate  *time.Time       `json:"publication_date,omitempty"`
	EffectiveDate    *time.Time       `json:"effective_date,omitempty"`

	// SourceURL is the official publication location, for verification only. A
	// citation never resolves through it.
	SourceURL string `json:"source_url,omitempty"`

	// Version is the published version this entry describes.
	Version VersionSummary `json:"version"`
}

// ListResult is a page of the catalogue.
type ListResult struct {
	Documents []DocumentSummary `json:"documents"`
	Total     int               `json:"total"`
	Limit     int               `json:"limit"`
	Offset    int               `json:"offset"`
}

// Detail is one document with every version a reader is allowed to see.
type Detail struct {
	Document DocumentSummary `json:"document"`

	// Versions is published first, then superseded ones, newest first. Drafts are
	// never included: unpublished text must not be readable through the browse
	// API any more than it is retrievable.
	Versions []VersionSummary `json:"versions"`
}

// Article is one provision, with its text.
type Article struct {
	ID           legal.ArticleID `json:"id"`
	ArticleNo    string          `json:"article_no"`
	ArticleNoRaw string          `json:"article_no_raw,omitempty"`
	Title        string          `json:"title,omitempty"`
	Chapter      string          `json:"chapter,omitempty"`
	SectionPath  []string        `json:"section_path,omitempty"`
	Text         string          `json:"text"`
	Ordinal      int             `json:"ordinal"`

	PageNo    int `json:"page_no"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`

	StructureConfidence legal.StructureConfidence `json:"structure_confidence"`

	// ChunkID is the article's first retrievable chunk. Carried so the reader can
	// bookmark or cite what they are looking at through the same identifiers an
	// answer would use.
	ChunkID legal.ChunkID `json:"chunk_id,omitempty"`
}

// ArticlePage is a slice of one version's articles, in document order.
type ArticlePage struct {
	Version  VersionSummary `json:"version"`
	Articles []Article      `json:"articles"`
	Total    int            `json:"total"`

	// NextAfter is the cursor for the next page, or nil at the end. Keyset
	// pagination on `ordinal`: a long code runs to a thousand articles and OFFSET
	// gets slower the deeper a reader scrolls.
	NextAfter *int `json:"next_after,omitempty"`
}

// LanguageStat and TypeStat break the corpus down for the catalogue header.
type LanguageStat struct {
	Language  legal.Language `json:"language"`
	Documents int            `json:"documents"`
	Articles  int            `json:"articles"`
}

type TypeStat struct {
	DocType   legal.DocType `json:"doc_type"`
	Documents int           `json:"documents"`
}

// Stats is what the corpus actually contains, counted over published versions
// only — the same set retrieval can see, so the number on the home page and the
// number of things answerable are the same number.
type Stats struct {
	Documents int `json:"documents"`
	Versions  int `json:"versions"`
	Articles  int `json:"articles"`
	Chunks    int `json:"chunks"`

	Languages []LanguageStat `json:"languages"`
	DocTypes  []TypeStat     `json:"doc_types"`

	LastPublishedAt *time.Time `json:"last_published_at,omitempty"`
}

// ListFilter is a validated catalogue query.
type ListFilter struct {
	// Term matches the title and issuing authority. Empty matches everything.
	Term     string
	Language legal.Language
	DocType  legal.DocType
	Limit    int
	Offset   int
}

// ParseListFilter reads and bounds the query string.
//
// Unknown values are rejected rather than ignored: a filter that silently does
// nothing looks to the user like a corpus that is missing documents.
func ParseListFilter(q url.Values) (ListFilter, error) {
	f := ListFilter{Limit: defaultPageSize}

	f.Term = strings.TrimSpace(q.Get("q"))
	if len([]rune(f.Term)) > maxSearchTermRunes {
		return ListFilter{}, fmt.Errorf("q is longer than %d characters", maxSearchTermRunes)
	}

	if raw := strings.TrimSpace(q.Get("language")); raw != "" {
		lang := legal.Language(raw)
		if !lang.Valid() {
			return ListFilter{}, fmt.Errorf("language %q is not one of en, am", raw)
		}
		f.Language = lang
	}

	if raw := strings.TrimSpace(q.Get("type")); raw != "" {
		docType := legal.DocType(raw)
		if !validDocType(docType) {
			return ListFilter{}, fmt.Errorf(
				"type %q is not one of proclamation, regulation, directive, code, other", raw)
		}
		f.DocType = docType
	}

	limit, err := intParam(q, "limit", defaultPageSize, 1, maxPageSize)
	if err != nil {
		return ListFilter{}, err
	}
	f.Limit = limit

	offset, err := intParam(q, "offset", 0, 0, 100_000)
	if err != nil {
		return ListFilter{}, err
	}
	f.Offset = offset

	return f, nil
}

// ArticleFilter is a validated request for a page of articles.
type ArticleFilter struct {
	// VersionID is empty for "the published version", which is what a reader who
	// followed a link without one means.
	VersionID legal.VersionID
	// After is the ordinal to continue from, exclusive.
	After int
	Limit int
}

func ParseArticleFilter(q url.Values) (ArticleFilter, error) {
	f := ArticleFilter{Limit: defaultArticlePageSize}

	if raw := strings.TrimSpace(q.Get("version")); raw != "" {
		f.VersionID = legal.VersionID(raw)
	}

	after, err := intParam(q, "after", 0, 0, 1_000_000)
	if err != nil {
		return ArticleFilter{}, err
	}
	f.After = after

	limit, err := intParam(q, "limit", defaultArticlePageSize, 1, maxArticlePageSize)
	if err != nil {
		return ArticleFilter{}, err
	}
	f.Limit = limit

	return f, nil
}

func intParam(q url.Values, name string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", name, raw)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return value, nil
}

func validDocType(t legal.DocType) bool {
	switch t {
	case legal.DocTypeProclamation, legal.DocTypeRegulation,
		legal.DocTypeDirective, legal.DocTypeCode, legal.DocTypeOther:
		return true
	default:
		return false
	}
}
