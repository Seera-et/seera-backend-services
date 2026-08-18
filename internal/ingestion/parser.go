// Package ingestion turns extracted legal documents into indexed, retrievable
// knowledge: parse, chunk, embed, store, publish.
//
// It does not parse PDFs. That happens offline in workers/extraction, whose
// JSON output this package consumes — see that directory's README for the
// contract both sides implement.
package ingestion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// Extraction is the JSON produced by workers/extraction/extract.py.
type Extraction struct {
	SourceFile string             `json:"source_file"`
	Language   string             `json:"language"`
	PageCount  int                `json:"page_count"`
	Articles   []ExtractedArticle `json:"articles"`
	Warnings   []string           `json:"warnings,omitempty"`
	Stats      map[string]any     `json:"stats,omitempty"`
}

// ExtractedArticle is one provision as the extractor found it.
type ExtractedArticle struct {
	ArticleNo           string   `json:"article_no"`
	ArticleNoRaw        string   `json:"article_no_raw"`
	Title               string   `json:"title"`
	Chapter             string   `json:"chapter"`
	SectionPath         []string `json:"section_path"`
	Text                string   `json:"text"`
	PageNo              int      `json:"page_no"`
	CharStart           int      `json:"char_start"`
	CharEnd             int      `json:"char_end"`
	StructureConfidence string   `json:"structure_confidence"`
}

// ParseExtraction decodes and validates an extraction.
//
// Validation is strict and refuses partial input. A malformed extraction that
// is ingested anyway becomes a document that answers questions wrongly, and
// nothing downstream will flag it — the retrieval layer cannot tell a truncated
// article from a short one.
func ParseExtraction(r io.Reader) (Extraction, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var e Extraction
	if err := dec.Decode(&e); err != nil {
		return Extraction{}, fmt.Errorf("decode extraction: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Extraction{}, err
	}
	return e, nil
}

// Validate checks an extraction against the contract.
func (e Extraction) Validate() error {
	var problems []error

	if !legal.Language(e.Language).Valid() {
		problems = append(problems, fmt.Errorf("language %q is not one of en, am", e.Language))
	}
	if len(e.Articles) == 0 {
		problems = append(problems, errors.New("extraction contains no articles"))
	}

	seen := make(map[string]int, len(e.Articles))
	for i, a := range e.Articles {
		where := fmt.Sprintf("article %d", i)
		if a.ArticleNo != "" {
			where = fmt.Sprintf("article %s (index %d)", a.ArticleNo, i)
		}

		if strings.TrimSpace(a.ArticleNo) == "" {
			problems = append(problems, fmt.Errorf("%s: article_no is empty", where))
		} else if prev, dup := seen[a.ArticleNo]; dup {
			// Duplicate numbers make citations ambiguous: two provisions would
			// resolve to the same reference.
			problems = append(problems, fmt.Errorf(
				"%s: article_no %q already used at index %d", where, a.ArticleNo, prev))
		} else {
			seen[a.ArticleNo] = i
		}

		if strings.TrimSpace(a.Text) == "" {
			problems = append(problems, fmt.Errorf("%s: text is empty", where))
		}
		switch a.StructureConfidence {
		case string(legal.StructureHigh), string(legal.StructureLow):
		case "":
			problems = append(problems, fmt.Errorf("%s: structure_confidence is missing", where))
		default:
			problems = append(problems, fmt.Errorf(
				"%s: structure_confidence %q is not one of high, low", where, a.StructureConfidence))
		}
		if a.CharEnd < a.CharStart {
			problems = append(problems, fmt.Errorf(
				"%s: char_end %d is before char_start %d", where, a.CharEnd, a.CharStart))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid extraction: %w", errors.Join(problems...))
	}
	return nil
}

// LowConfidenceCount reports how many articles fell back to window chunking.
// The caller logs it: a document that is mostly low-confidence parsed badly,
// and that is worth knowing before it starts answering questions.
func (e Extraction) LowConfidenceCount() int {
	var n int
	for _, a := range e.Articles {
		if a.StructureConfidence == string(legal.StructureLow) {
			n++
		}
	}
	return n
}

// toArticles converts extracted articles into domain articles, assigning
// ordinals from the extraction order.
func (e Extraction) toArticles(docID legal.DocumentID, versionID legal.VersionID) []legal.Article {
	articles := make([]legal.Article, 0, len(e.Articles))
	for i, a := range e.Articles {
		raw := a.ArticleNoRaw
		if raw == "" {
			raw = a.ArticleNo
		}
		articles = append(articles, legal.Article{
			VersionID:           versionID,
			DocumentID:          docID,
			Number:              strings.TrimSpace(a.ArticleNo),
			NumberRaw:           raw,
			Title:               strings.TrimSpace(a.Title),
			Chapter:             strings.TrimSpace(a.Chapter),
			SectionPath:         a.SectionPath,
			Text:                strings.TrimSpace(a.Text),
			Ordinal:             i,
			PageNo:              a.PageNo,
			CharStart:           a.CharStart,
			CharEnd:             a.CharEnd,
			StructureConfidence: legal.StructureConfidence(a.StructureConfidence),
		})
	}
	return articles
}
