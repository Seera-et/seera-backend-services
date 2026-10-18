package corpus

import (
	"errors"
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

func date(t *testing.T, s string) *time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return &d
}

func extraction(lang string) ingestion.Extraction {
	return ingestion.Extraction{
		Language:  lang,
		PageCount: 10,
		Articles: []ingestion.ExtractedArticle{{
			ArticleNo: "1", Text: "A provision.", StructureConfidence: "high",
		}},
	}
}

func record() crawler.Record {
	return crawler.Record{
		DocID:    "federal-courts-proclamation-9f3a1c2b",
		State:    crawler.StateReady,
		Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Page: crawler.LawPage{
			Ref:             crawler.LawPageRef{URL: "https://justice.gov.et/en/law/federal-courts"},
			TitleEn:         "Federal Courts Proclamation",
			TitleAm:         "የፌዴራል ፍርድ ቤቶች አዋጅ",
			DocumentNumber:  "1234/2021",
			DocType:         legal.DocTypeProclamation,
			PublicationDate: nil,
			Status:          "In Force",
		},
	}
}

func TestPlanRequestTakesTitleFromTheDocumentsLanguage(t *testing.T) {
	for _, tc := range []struct{ lang, want string }{
		{"en", "Federal Courts Proclamation"},
		{"am", "የፌዴራል ፍርድ ቤቶች አዋጅ"},
	} {
		req, err := PlanRequest(record(), extraction(tc.lang))
		if err != nil {
			t.Fatalf("PlanRequest(%q) = %v", tc.lang, err)
		}
		if req.Document.Title != tc.want {
			t.Errorf("language %q: title = %q, want %q", tc.lang, req.Document.Title, tc.want)
		}
		if string(req.Document.Language) != tc.lang {
			t.Errorf("language %q: document language = %q", tc.lang, req.Document.Language)
		}
	}
}

// The Ministry's English pages routinely link Amharic PDFs, and a page may
// carry only one half of its bilingual title. Ingesting with an empty title is
// not a cosmetic problem: documents resolve by (lower(title), language), so the
// SECOND empty-titled document matches the first and attaches its version to
// it. Two unrelated laws become one identity, and publishing the second
// supersedes the first.
func TestPlanRequestRefusesAnEmptyTitleRatherThanMergingDocuments(t *testing.T) {
	rec := record()
	rec.Page.TitleEn = ""

	if _, err := PlanRequest(rec, extraction("en")); !errors.Is(err, errNoTitle) {
		t.Fatalf("PlanRequest with no English title = %v, want errNoTitle", err)
	}

	// The Amharic half is present, so the same record in Amharic is fine.
	if _, err := PlanRequest(rec, extraction("am")); err != nil {
		t.Errorf("PlanRequest with an Amharic title = %v, want success", err)
	}
}

// docTypeFrom only runs when the page matched a pattern, so the zero value is
// reachable — and ” fails legal_documents_doc_type_check after the document
// has already been embedded.
func TestPlanRequestDefaultsAnUnknownDocTypeRatherThanFailingLater(t *testing.T) {
	rec := record()
	rec.Page.DocType = ""

	req, err := PlanRequest(rec, extraction("en"))
	if err != nil {
		t.Fatalf("PlanRequest() = %v", err)
	}
	if req.Document.DocType != legal.DocTypeOther {
		t.Errorf("DocType = %q, want %q", req.Document.DocType, legal.DocTypeOther)
	}
}

func TestVersionLabelPrefersThePrintedNumber(t *testing.T) {
	rec := record()
	rec.Page.PublicationDate = date(t, "2021-04-26")

	if got := versionLabel(rec); got != "1234/2021" {
		t.Errorf("versionLabel() = %q, want the printed number", got)
	}
}

func TestVersionLabelFallsBackToYearThenChecksum(t *testing.T) {
	rec := record()
	rec.Page.DocumentNumber = ""
	rec.Page.PublicationDate = date(t, "2021-04-26")
	if got := versionLabel(rec); got != "2021" {
		t.Errorf("versionLabel() = %q, want the publication year", got)
	}

	rec.Page.PublicationDate = nil
	if got := versionLabel(rec); got != "sha-e3b0c442" {
		t.Errorf("versionLabel() = %q, want the checksum prefix", got)
	}
}

// Provenance belongs to the version, because ingestion reuses an existing
// document row without rewriting it — so a document-level source URL only ever
// describes whichever version happened to be ingested first.
func TestPlanRequestPutsProvenanceOnTheVersion(t *testing.T) {
	rec := record()
	req, err := PlanRequest(rec, extraction("en"))
	if err != nil {
		t.Fatalf("PlanRequest() = %v", err)
	}

	if req.SourceChecksum != rec.Checksum {
		t.Errorf("SourceChecksum = %q, want the crawl checksum", req.SourceChecksum)
	}
	if req.Version.SourceURL != rec.Page.Ref.URL {
		t.Errorf("Version.SourceURL = %q, want %q", req.Version.SourceURL, rec.Page.Ref.URL)
	}
	if req.Version.StorageKey != rec.DocID {
		t.Errorf("Version.StorageKey = %q, want the crawler doc id", req.Version.StorageKey)
	}
}

// Ministry law pages do not state effective dates. Inferring one changes which
// law the in-force filter returns, so these stay nil and the publish decision
// carries the weight instead.
func TestPlanRequestNeverInventsDatesOrAnAuthority(t *testing.T) {
	req, err := PlanRequest(record(), extraction("en"))
	if err != nil {
		t.Fatalf("PlanRequest() = %v", err)
	}
	if req.Version.EffectiveFrom != nil || req.Version.RepealedAt != nil {
		t.Error("invented a version effective/repealed date")
	}
	if req.Document.EffectiveDate != nil {
		t.Error("invented a document effective date")
	}
	if req.Document.IssuingAuthority != "" {
		t.Errorf("invented an issuing authority %q — it is embedded into every chunk header",
			req.Document.IssuingAuthority)
	}
}
