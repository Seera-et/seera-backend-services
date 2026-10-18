package corpus

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// errNoTitle is returned when the crawl record carries no title in the
// document's own language.
//
// Refused rather than defaulted, because an empty title is not merely missing
// metadata — ingestion resolves documents by (lower(title), language), so the
// second empty-titled document *matches* the first and attaches its version to
// it. Two unrelated laws become one identity, publishing the second supersedes
// the first, and every chunk is embedded under an empty context header.
var errNoTitle = errors.New("crawl record has no title in the document's language")

// PlanRequest turns a crawl record and its extraction into an ingestion
// request.
//
// Nothing here is inferred. Every field is either on the record, derived from
// the extraction, or left empty — which is the same rule internal/crawler
// already applies to its own parsing, and for the same reason: a guessed
// effective date or document number produces a citation that looks
// authoritative and is wrong.
//
// Notably absent: effective_from, effective_date and repealed_at are always
// nil. Ministry law pages do not state them. A null effective_from reads as
// "in force" to the retrieval filter, which is permissive — so the publish
// decision carries the weight instead, and a record the Ministry marks repealed
// is never published at all (see Gates).
func PlanRequest(rec crawler.Record, ext ingestion.Extraction) (ingestion.Request, error) {
	// The document's language comes from the extracted text, never from the
	// page. The Ministry's English pages routinely link Amharic PDFs, and
	// ingestion refuses a mismatch — so taking the page's word for it turns a
	// correct extraction into a failed ingest.
	lang := legal.Language(ext.Language)

	title := strings.TrimSpace(rec.Page.TitleEn)
	if lang == legal.LanguageAmharic {
		title = strings.TrimSpace(rec.Page.TitleAm)
	}
	if title == "" {
		return ingestion.Request{}, fmt.Errorf("%w: %s (language %q)", errNoTitle, rec.DocID, lang)
	}

	// docTypeFrom defaults unknown instruments to "other", but it only runs
	// when the page matched a pattern at all — so the zero value is reachable
	// and would fail the doc_type check after embedding had been paid for.
	docType := rec.Page.DocType
	if docType == "" {
		docType = legal.DocTypeOther
	}

	return ingestion.Request{
		Document: legal.Document{
			Title:           title,
			DocType:         docType,
			Language:        lang,
			PublicationDate: rec.Page.PublicationDate,
			SourceURL:       rec.Page.Ref.URL,
			StorageKey:      rec.DocID,
			// IssuingAuthority is deliberately empty: Ministry law pages do not
			// state it, and the chunker writes it into every chunk's embedded
			// context header, so a plausible guess would be embedded into the
			// corpus hundreds of times over.
		},
		Version: legal.Version{
			Label:      versionLabel(rec),
			SourceURL:  rec.Page.Ref.URL,
			StorageKey: rec.DocID,
		},
		SourceChecksum: rec.Checksum,
		Extraction:     ext,
	}, nil
}

// versionLabel names this edition of the document.
//
// Preference order matters. The printed document number is the label a lawyer
// would use and is stable across re-crawls. The publication year is a weaker
// but still stable fallback. The checksum prefix is last and is explicitly
// unstable: if the Ministry re-saves the same law the bytes change, so the
// label changes, and the document gains a second version that is really the
// same text. That is a visible, inspectable outcome — unlike fabricating
// "1156/2019-b", which would mint a citation that looks authoritative and
// refers to nothing.
func versionLabel(rec crawler.Record) string {
	if n := strings.TrimSpace(rec.Page.DocumentNumber); n != "" {
		return n
	}
	if rec.Page.PublicationDate != nil {
		return strconv.Itoa(rec.Page.PublicationDate.Year())
	}
	if len(rec.Checksum) >= 8 {
		return "sha-" + rec.Checksum[:8]
	}
	return ""
}
