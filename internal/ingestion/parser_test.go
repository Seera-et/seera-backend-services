package ingestion

import (
	"strings"
	"testing"
)

const validExtraction = `{
  "source_file": "commercial_code.pdf",
  "language": "en",
  "page_count": 412,
  "articles": [
    {
      "article_no": "45",
      "article_no_raw": "45",
      "title": "Formation of private limited companies",
      "chapter": "Companies",
      "section_path": ["Book II", "Chapter 3"],
      "text": "A private limited company may be formed by two or more persons.",
      "page_no": 88,
      "char_start": 145203,
      "char_end": 147891,
      "structure_confidence": "high"
    }
  ]
}`

func TestParseExtractionAcceptsTheContract(t *testing.T) {
	e, err := ParseExtraction(strings.NewReader(validExtraction))
	if err != nil {
		t.Fatalf("ParseExtraction() = %v", err)
	}
	if e.Language != "en" || len(e.Articles) != 1 {
		t.Fatalf("parsed %+v, want one English article", e)
	}
	if e.Articles[0].ArticleNo != "45" {
		t.Errorf("article_no = %q, want 45", e.Articles[0].ArticleNo)
	}
}

// An unknown field means the extractor and the ingester have drifted apart.
// Ignoring it silently drops data that one side believed it had sent.
func TestParseExtractionRejectsUnknownFields(t *testing.T) {
	body := strings.Replace(validExtraction, `"page_count": 412,`, `"page_count": 412, "pages": 412,`, 1)

	if _, err := ParseExtraction(strings.NewReader(body)); err == nil {
		t.Error("ParseExtraction() accepted an unknown field")
	}
}

func TestValidateRejectsBadExtractions(t *testing.T) {
	base := func() Extraction {
		e, err := ParseExtraction(strings.NewReader(validExtraction))
		if err != nil {
			t.Fatalf("fixture is invalid: %v", err)
		}
		return e
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*Extraction)
		wantSub string
	}{
		{
			name:    "unsupported language",
			mutate:  func(e *Extraction) { e.Language = "fr" },
			wantSub: "language",
		},
		{
			name:    "no articles",
			mutate:  func(e *Extraction) { e.Articles = nil },
			wantSub: "no articles",
		},
		{
			name:    "empty article number",
			mutate:  func(e *Extraction) { e.Articles[0].ArticleNo = "" },
			wantSub: "article_no is empty",
		},
		{
			name:    "empty text",
			mutate:  func(e *Extraction) { e.Articles[0].Text = "   " },
			wantSub: "text is empty",
		},
		{
			name:    "missing confidence",
			mutate:  func(e *Extraction) { e.Articles[0].StructureConfidence = "" },
			wantSub: "structure_confidence is missing",
		},
		{
			name:    "unknown confidence",
			mutate:  func(e *Extraction) { e.Articles[0].StructureConfidence = "medium" },
			wantSub: "structure_confidence",
		},
		{
			name:    "inverted span",
			mutate:  func(e *Extraction) { e.Articles[0].CharEnd = e.Articles[0].CharStart - 1 },
			wantSub: "char_end",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			tc.mutate(&e)

			err := e.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted an extraction with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// Two provisions under one number make every citation to it ambiguous, and the
// citation resolver has no way to pick the right one.
func TestValidateRejectsDuplicateArticleNumbers(t *testing.T) {
	e, err := ParseExtraction(strings.NewReader(validExtraction))
	if err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	e.Articles = append(e.Articles, e.Articles[0])

	if err := e.Validate(); err == nil {
		t.Fatal("Validate() accepted a duplicate article number")
	} else if !strings.Contains(err.Error(), "already used") {
		t.Errorf("error %q does not explain the duplicate", err)
	}
}

func TestLowConfidenceCount(t *testing.T) {
	e, err := ParseExtraction(strings.NewReader(validExtraction))
	if err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	if got := e.LowConfidenceCount(); got != 0 {
		t.Errorf("LowConfidenceCount() = %d, want 0", got)
	}

	low := e.Articles[0]
	low.ArticleNo = "window-1"
	low.StructureConfidence = "low"
	e.Articles = append(e.Articles, low)

	if got := e.LowConfidenceCount(); got != 1 {
		t.Errorf("LowConfidenceCount() = %d, want 1", got)
	}
}

func TestToArticlesAssignsOrdinalsInDocumentOrder(t *testing.T) {
	e, err := ParseExtraction(strings.NewReader(validExtraction))
	if err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	second := e.Articles[0]
	second.ArticleNo = "46"
	second.ArticleNoRaw = ""
	e.Articles = append(e.Articles, second)

	articles := e.toArticles("doc-1", "ver-1")

	if len(articles) != 2 {
		t.Fatalf("got %d articles, want 2", len(articles))
	}
	for i, a := range articles {
		if a.Ordinal != i {
			t.Errorf("article %s has ordinal %d, want %d", a.Number, a.Ordinal, i)
		}
		if a.DocumentID != "doc-1" || a.VersionID != "ver-1" {
			t.Errorf("article %s was not linked to its document and version", a.Number)
		}
	}
	// An absent raw form falls back to the normalised number rather than being
	// left blank, so the citation always has something to print.
	if articles[1].NumberRaw != "46" {
		t.Errorf("NumberRaw = %q, want the fallback to article_no", articles[1].NumberRaw)
	}
}
