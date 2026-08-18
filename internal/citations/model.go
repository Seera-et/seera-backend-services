// Package citations resolves the chunk identifiers a generated answer refers to
// into full, user-facing legal references.
//
// A citation always points at internal database records. It is never a URL, a
// page number the parser did not emit, or a string the model wrote. The
// frontend renders from resolved Citation values — never by parsing the answer
// text.
package citations

import (
	"context"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Citation is a resolved reference: everything the UI needs to show a source
// card and open the exact provision the claim rests on.
//
// The JSON names are snake_case and deliberately match rag.Citation's, because
// both describe the same thing to the same frontend: rag.Citation arrives on the
// answer stream, this one from GET /api/v1/citations/{id}. Without the tags Go
// marshals these as PascalCase, and a client would need two shapes for one
// concept — caught by actually calling the endpoint.
type Citation struct {
	ChunkID    legal.ChunkID    `json:"chunk_id"`
	ArticleID  legal.ArticleID  `json:"article_id"`
	VersionID  legal.VersionID  `json:"version_id"`
	DocumentID legal.DocumentID `json:"document_id"`

	DocumentTitle    string         `json:"document_title"`
	IssuingAuthority string         `json:"issuing_authority,omitempty"`
	ArticleNo        string         `json:"article_no"`
	ArticleTitle     string         `json:"article_title,omitempty"`
	SectionPath      []string       `json:"section_path,omitempty"`
	Language         legal.Language `json:"language"`

	// Version provenance. An answer must be able to state which version of the
	// law it relied on and from when that version applied.
	VersionLabel  string     `json:"version_label"`
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	RepealedAt    *time.Time `json:"repealed_at,omitempty"`

	// Text is the exact source text of the cited chunk, verbatim.
	Text string `json:"text"`

	// Location in the original document, for highlighting in the source viewer.
	PageNo    int `json:"page_no"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`

	// SourceURL is the official publication location, retained for verification.
	// It is never what the citation resolves through.
	SourceURL string `json:"source_url,omitempty"`
}

// Resolver turns chunk IDs into citations.
//
// Resolve returns citations in the order requested and omits nothing silently:
// an ID that does not resolve is an error, because an unresolvable citation in
// a delivered answer is a grounding failure, not a display glitch.
type Resolver interface {
	Resolve(ctx context.Context, ids []legal.ChunkID) ([]Citation, error)
}
