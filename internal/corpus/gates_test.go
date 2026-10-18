package corpus

import (
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/ingestion"
)

// cleanExtraction is a document that passes every gate, so each test can spoil
// exactly one thing and attribute the failure to it.
func cleanExtraction() ingestion.Extraction {
	articles := make([]ingestion.ExtractedArticle, 50)
	for i := range articles {
		articles[i] = ingestion.ExtractedArticle{
			ArticleNo: string(rune('0'+i/10)) + string(rune('0'+i%10)),
			Text:      "A provision.", StructureConfidence: "high",
		}
	}
	return ingestion.Extraction{
		Language:  "en",
		PageCount: 100,
		Articles:  articles,
		Stats: map[string]any{
			"characters":               100000.0,
			"article_characters":       90000.0,
			"pages_without_text_layer": 0.0,
			"article_gaps":             0.0,
			"article_number_min":       1.0,
			"article_number_max":       50.0,
			"ethiopic_ratio":           0.0,
			"structure":                "article",
		},
	}
}

func assertFails(t *testing.T, v Verdict, substring string) {
	t.Helper()
	if v.OK() {
		t.Fatalf("gates passed; expected a failure mentioning %q", substring)
	}
	if !strings.Contains(v.Reason(), substring) {
		t.Errorf("failure was %q, expected it to mention %q", v.Reason(), substring)
	}
}

func TestCleanDocumentPassesEveryGate(t *testing.T) {
	v := Evaluate(record(), cleanExtraction(), DefaultThresholds())
	if !v.OK() {
		t.Fatalf("a clean document failed: %s", v.Reason())
	}
}

// The gate that matters most, and the data has been on the crawl record all
// along. A repealed proclamation's page states no repeal date, so
// legal.Version.InForceAt reads a nil RepealedAt as in force — publish it and
// the answer path cites repealed law as current.
func TestRepealedLawIsNeverPublished(t *testing.T) {
	for _, status := range []string{"Repealed", "የተሻረ"} {
		rec := record()
		rec.Page.Status = status
		assertFails(t, Evaluate(rec, cleanExtraction(), DefaultThresholds()), "repealed")
	}
}

func TestAmendedLawIsNeverPublished(t *testing.T) {
	for _, status := range []string{"Amended", "የተሻሻለ"} {
		rec := record()
		rec.Page.Status = status
		assertFails(t, Evaluate(rec, cleanExtraction(), DefaultThresholds()), "amended")
	}
}

func TestInForceAndUnknownStatusesPass(t *testing.T) {
	for _, status := range []string{"In Force", "በአገልግሎት ላይ", ""} {
		rec := record()
		rec.Page.Status = status
		if v := Evaluate(rec, cleanExtraction(), DefaultThresholds()); !v.OK() {
			t.Errorf("status %q was held back: %s", status, v.Reason())
		}
	}
}

// Window fallback means no article structure was found at all. Those chunks
// cite nothing useful, and one check covers it because the extractor marks
// every article high or every one low — there is no mixed case.
func TestWindowFallbackIsNeverPublished(t *testing.T) {
	ext := cleanExtraction()
	for i := range ext.Articles {
		ext.Articles[i].StructureConfidence = "low"
	}
	assertFails(t, Evaluate(record(), ext, DefaultThresholds()), "window fallback")
}

// The gate OCR's absence makes necessary. A 300-page scan with five incidental
// text pages passes the extractor's text-quality check — those five pages are
// real text — and would otherwise ingest as a five-article "code".
func TestAScanWithAFewTextPagesIsHeldBack(t *testing.T) {
	ext := cleanExtraction()
	ext.PageCount = 300
	ext.Stats["pages_without_text_layer"] = 295.0

	assertFails(t, Evaluate(record(), ext, DefaultThresholds()), "no text layer")
}

// Coverage replaces articles-per-page, which cannot work: any floor loose
// enough to pass a 2-page directive with one article also passes a 500-page
// code where only forty articles were found.
func TestLowTextCoverageIsHeldBack(t *testing.T) {
	ext := cleanExtraction()
	ext.Stats["article_characters"] = 10000.0 // 10% of 100000

	assertFails(t, Evaluate(record(), ext, DefaultThresholds()), "inside an article")
}

func TestCoverageGateAcceptsAShortDocumentWithOneArticle(t *testing.T) {
	ext := cleanExtraction()
	ext.PageCount = 3
	ext.Articles = ext.Articles[:1]
	ext.Stats["characters"] = 4000.0
	ext.Stats["article_characters"] = 3600.0
	ext.Stats["article_number_max"] = 1.0

	// A loan-ratification proclamation: 3 pages, 1 article. Genuinely the whole
	// document, and an articles-per-page floor would have quarantined every one.
	if v := Evaluate(record(), ext, DefaultThresholds()); !v.OK() {
		t.Errorf("a short single-article proclamation was held back: %s", v.Reason())
	}
}

func TestManyMissingArticleNumbersIsHeldBack(t *testing.T) {
	ext := cleanExtraction()
	ext.Stats["article_gaps"] = 40.0 // 40 missing against 50 found
	ext.Stats["article_number_max"] = 90.0

	v := Evaluate(record(), ext, DefaultThresholds())
	assertFails(t, v, "article numbers missing")
	// The bounds are in the message, because one misread page number becoming
	// article "999" produces hundreds of spurious gaps and the operator has to
	// be able to see that rather than guess.
	if !strings.Contains(v.Reason(), "between 1 and 90") {
		t.Errorf("gap failure %q does not report the numbering range", v.Reason())
	}
}

// An Amharic PDF extracted as English is stored under language 'en', which
// makes Postgres index Ethiopic with the English stemmer and the chunker write
// an English context header onto Amharic provisions. Nothing fails; it just
// retrieves badly forever.
func TestScriptMismatchWithTheDeclaredLanguageIsHeldBack(t *testing.T) {
	ext := cleanExtraction() // language "en"
	ext.Stats["ethiopic_ratio"] = 0.95

	assertFails(t, Evaluate(record(), ext, DefaultThresholds()), "does not look like")
}

func TestBilingualDocumentsAreNotHeldBack(t *testing.T) {
	ext := cleanExtraction()
	ext.Stats["ethiopic_ratio"] = 0.35 // an English code quoting Amharic terms

	if v := Evaluate(record(), ext, DefaultThresholds()); !v.OK() {
		t.Errorf("a bilingual document was held back: %s", v.Reason())
	}
}

func TestAmharicDocumentPassesTheScriptGate(t *testing.T) {
	ext := cleanExtraction()
	ext.Language = "am"
	ext.Stats["ethiopic_ratio"] = 0.92

	if v := Evaluate(record(), ext, DefaultThresholds()); !v.OK() {
		t.Errorf("an Amharic document was held back: %s", v.Reason())
	}
}

// An extraction produced before the stats keys existed must not be published on
// the strength of stats that are simply absent — but neither should a missing
// key invent a failure. Zero reads as "no evidence of a problem", and the
// structure gate still protects the corpus.
func TestMissingStatsDoNotFabricateFailures(t *testing.T) {
	ext := cleanExtraction()
	ext.Stats = nil

	if v := Evaluate(record(), ext, DefaultThresholds()); !v.OK() {
		t.Errorf("an extraction without stats failed on absent data: %s", v.Reason())
	}
}

func TestEveryFailureIsReported(t *testing.T) {
	rec := record()
	rec.Page.Status = "Repealed"
	ext := cleanExtraction()
	ext.Stats["article_characters"] = 1000.0
	for i := range ext.Articles {
		ext.Articles[i].StructureConfidence = "low"
	}

	v := Evaluate(rec, ext, DefaultThresholds())
	if len(v.Failures) < 3 {
		t.Errorf("got %d failures, want all of them: %v", len(v.Failures), v.Failures)
	}
}
