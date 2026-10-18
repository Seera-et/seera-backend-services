package corpus

import (
	"fmt"
	"strings"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
	"github.com/Seera-et/seera-backend-services/internal/ingestion"
)

// Thresholds are the publish gates' tunable limits.
//
// Exposed as flags so a corpus that turns out to parse differently can be
// retuned without a release. The defaults are calibrated against the documents
// measured in docs/CORPUS.md.
type Thresholds struct {
	// MinCoverage is the share of extracted characters that must fall inside a
	// detected article.
	//
	// This replaces the obvious "articles per page", which cannot work: any
	// floor loose enough to pass a two-page directive with one article also
	// passes a 500-page code where only forty articles were found. Coverage
	// measures the thing actually at stake — how much of the document ended up
	// inside a citable provision — and is scale-free.
	//
	// It never reaches 1.0: duplicate headings and tables of contents are
	// dropped during extraction, and front matter is not a provision.
	MinCoverage float64

	// MaxGapRatio is how many article numbers inside the document's own range
	// may be missing, relative to the articles found.
	//
	// A document whose markers were half-missed looks healthy by every other
	// measure and silently answers questions with the provisions that survived.
	MaxGapRatio float64

	// MaxPagesWithoutText is the share of pages that may have no text layer.
	//
	// The most valuable gate here, because OCR is not wired up: a 300-page scan
	// carrying five incidental text pages passes the extractor's text-quality
	// check — those five pages are genuine text and score well — and would be
	// ingested as a five-article "code". Nothing downstream could tell.
	MaxPagesWithoutText float64

	// MinLanguageAgreement is how strongly the text's script must agree with
	// the declared language.
	//
	// An Amharic PDF extracted as English is stored with language 'en', which
	// makes Postgres index Ethiopic with the English stemmer (migration 0002)
	// and makes the chunker write an English context header onto Amharic
	// provisions. It does not fail; it just retrieves badly, forever.
	//
	// Loose, because bilingual documents are real and common here.
	MinLanguageAgreement float64
}

// DefaultThresholds are calibrated against docs/CORPUS.md.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinCoverage:          0.50,
		MaxGapRatio:          0.10,
		MaxPagesWithoutText:  0.10,
		MinLanguageAgreement: 0.50,
	}
}

// Verdict is the outcome of evaluating one document's gates.
type Verdict struct {
	// Failures is empty when the document may be published.
	Failures []string
}

// OK reports whether every gate passed.
func (v Verdict) OK() bool { return len(v.Failures) == 0 }

// Reason is a one-line summary for the report.
func (v Verdict) Reason() string {
	if v.OK() {
		return ""
	}
	return strings.Join(v.Failures, "; ")
}

// Ministry status labels, English and Amharic, exactly as lawpage.go matches
// them.
const (
	statusRepealed   = "Repealed"
	statusRepealedAm = "የተሻረ"
	statusAmended    = "Amended"
	statusAmendedAm  = "የተሻሻለ"
)

// Evaluate decides whether a document is fit to publish without a human having
// read the parse.
//
// Failing a gate is never destructive. The document is still ingested, as a
// draft, so it is in the database and inspectable; it is simply not visible to
// retrieval. The asymmetry is deliberate: a document missing from the corpus
// produces "I could not find this", while a badly parsed one produces a
// confident answer citing provisions that were never read correctly.
func Evaluate(rec crawler.Record, ext ingestion.Extraction, t Thresholds) Verdict {
	var v Verdict
	fail := func(format string, args ...any) {
		v.Failures = append(v.Failures, fmt.Sprintf(format, args...))
	}

	// 1. The Ministry's own status. This is the gate that matters most and the
	//    data has been on the record all along. A repealed proclamation has no
	//    repealed_at date on its page, so legal.Version.InForceAt would read it
	//    as in force and the answer path would cite it as current law.
	switch strings.TrimSpace(rec.Page.Status) {
	case statusRepealed, statusRepealedAm:
		fail("the Ministry marks this law repealed")
	case statusAmended, statusAmendedAm:
		// Not wrong to hold, but a newer instrument exists and we do not know
		// which. Publishing this one asserts it is the current text.
		fail("the Ministry marks this law amended; a newer instrument exists")
	}

	// 2. Structure. build_articles marks every article high and build_windows
	//    marks every one low, so this single check covers "fell back to fixed
	//    windows", "found fewer than three articles", and every mixed case.
	//    Window chunks cite nothing useful and retrieve worse.
	if n := ext.LowConfidenceCount(); n > 0 {
		fail("%d of %d articles came from window fallback, not real structure",
			n, len(ext.Articles))
	}

	// 3. Pages with no text layer.
	if ext.PageCount > 0 {
		blank := statInt(ext.Stats, "pages_without_text_layer")
		if ratio := float64(blank) / float64(ext.PageCount); ratio > t.MaxPagesWithoutText {
			fail("%d of %d pages have no text layer (%.0f%%, limit %.0f%%) — this document needs OCR",
				blank, ext.PageCount, ratio*100, t.MaxPagesWithoutText*100)
		}
	}

	// 4. Coverage: how much of the text landed inside a provision.
	if chars := statInt(ext.Stats, "characters"); chars > 0 {
		covered := statInt(ext.Stats, "article_characters")
		if ratio := float64(covered) / float64(chars); ratio < t.MinCoverage {
			fail("only %.0f%% of the text is inside an article (floor %.0f%%) — "+
				"provisions are being missed", ratio*100, t.MinCoverage*100)
		}
	}

	// 5. Gaps in the document's own article numbering.
	if len(ext.Articles) > 0 {
		gaps := statInt(ext.Stats, "article_gaps")
		if ratio := float64(gaps) / float64(len(ext.Articles)); ratio > t.MaxGapRatio {
			// The raw bounds go in the message because one misread page number
			// becoming article "999" turns a clean parse into hundreds of
			// reported gaps, and the operator needs to see that rather than
			// guess at it.
			fail("%d article numbers missing between %d and %d across %d articles (limit %.0f%%)",
				gaps, statInt(ext.Stats, "article_number_min"),
				statInt(ext.Stats, "article_number_max"), len(ext.Articles), t.MaxGapRatio*100)
		}
	}

	// 6. Script against declared language.
	if ratio, ok := statFloat(ext.Stats, "ethiopic_ratio"); ok {
		agreement := ratio
		if ext.Language == "en" {
			agreement = 1 - ratio
		}
		if agreement < t.MinLanguageAgreement {
			fail("text does not look like %q (Ethiopic share %.0f%%) — "+
				"check the --language passed to extract.py", ext.Language, ratio*100)
		}
	}

	return v
}

// statInt reads an integer out of the extraction's stats map.
//
// JSON numbers decode as float64 through map[string]any, and a stat the
// extractor did not emit is simply absent — an older extraction run against a
// newer binary is a normal situation, not an error. A missing stat reads as
// zero, and every gate above is written so that zero cannot fail it on its own.
func statInt(stats map[string]any, key string) int {
	v, _ := statFloat(stats, key)
	return int(v)
}

func statFloat(stats map[string]any, key string) (float64, bool) {
	switch v := stats[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}
