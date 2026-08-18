package ingestion

import (
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

func testDocument() legal.Document {
	return legal.Document{
		Title:            "Commercial Code of Ethiopia",
		IssuingAuthority: "Federal Democratic Republic of Ethiopia",
		Language:         legal.LanguageEnglish,
	}
}

func testArticle(text string) legal.Article {
	return legal.Article{
		Number:      "45",
		Title:       "Formation of private limited companies",
		SectionPath: []string{"Book II", "Chapter 3 — Companies"},
		Text:        text,
	}
}

// The whole reason EmbedText exists. A sub-article embedded on its own is
// unretrievable, because nothing in it says which period, under which law.
func TestChunkEmbedTextCarriesProvenance(t *testing.T) {
	chunks, err := Chunk(testDocument(), testArticle("(2) The period shall be thirty days."))
	if err != nil {
		t.Fatalf("Chunk() = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}

	embed := chunks[0].EmbedText
	for _, want := range []string{
		"Commercial Code of Ethiopia",
		"Chapter 3 — Companies",
		"Article 45",
		"Formation of private limited companies",
		"The period shall be thirty days.",
	} {
		if !strings.Contains(embed, want) {
			t.Errorf("EmbedText is missing %q:\n%s", want, embed)
		}
	}

	// Text is what the user is shown and what a citation quotes, so it must
	// stay verbatim — the header belongs only in the embedded form.
	if chunks[0].Text != "(2) The period shall be thirty days." {
		t.Errorf("Text = %q, want the raw span unchanged", chunks[0].Text)
	}
}

func TestChunkKeepsOneChunkPerArticle(t *testing.T) {
	article := testArticle(strings.Repeat("A private limited company may be formed. ", 20))

	chunks, err := Chunk(testDocument(), article)
	if err != nil {
		t.Fatalf("Chunk() = %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("got %d chunks for a short article, want 1 — a chunk should be a "+
			"complete provision, not a window", len(chunks))
	}
}

func TestChunkSplitsLongArticlesAtSubArticleBoundaries(t *testing.T) {
	// Six sub-articles, each well over a quarter of the limit.
	var b strings.Builder
	body := strings.Repeat("The provisions of this Article shall apply accordingly. ", 60)
	for i := 1; i <= 6; i++ {
		b.WriteString("(")
		b.WriteString(string(rune('0' + i)))
		b.WriteString(") ")
		b.WriteString(body)
		b.WriteString("\n")
	}

	chunks, err := Chunk(testDocument(), testArticle(b.String()))
	if err != nil {
		t.Fatalf("Chunk() = %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the article split", len(chunks))
	}

	for i, c := range chunks {
		if c.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, c.Ordinal)
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Text), "(") {
			t.Errorf("chunk %d does not start at a sub-article boundary: %.60q", i, c.Text)
		}
		if !strings.Contains(c.EmbedText, "Article 45") {
			t.Errorf("chunk %d lost its context header", i)
		}
		if got := EstimateTokens(c.Text); got > MaxChunkTokens {
			t.Errorf("chunk %d is %d tokens, over the %d limit", i, got, MaxChunkTokens)
		}
	}
}

func TestChunkRejectsEmptyArticle(t *testing.T) {
	if _, err := Chunk(testDocument(), testArticle("   \n  ")); err == nil {
		t.Error("Chunk() accepted an article with no text")
	}
}

func TestContextHeaderFallsBackToChapterWhenNoSectionPath(t *testing.T) {
	article := testArticle("Text.")
	article.SectionPath = nil
	article.Chapter = "Companies"

	if got := ContextHeader(testDocument(), article); !strings.Contains(got, "Companies") {
		t.Errorf("header = %q, want it to carry the chapter", got)
	}
}

// Ethiopic tokenises far worse than Latin under byte-pair encodings trained
// mostly on English. Counting it at parity would under-split Amharic articles
// into chunks well over the model's context.
func TestEstimateTokensCountsEthiopicHigherThanLatin(t *testing.T) {
	latin := EstimateTokens(strings.Repeat("a", 100))
	ethiopic := EstimateTokens(strings.Repeat("ሀ", 100))

	if ethiopic <= latin {
		t.Errorf("Ethiopic estimate %d is not above Latin %d for the same length",
			ethiopic, latin)
	}
	if EstimateTokens("") != 0 {
		t.Error("empty string should estimate zero tokens")
	}
}

func TestStartsSubArticle(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"(1) The company shall...", true},
		{"1) The company shall...", true},
		{"2. The company shall...", true},
		{"(1a) An amended provision", true},
		{"፩) የኩባንያው", true},
		{"The company shall...", false},
		{"(see Article 12)", false},
		{"", false},
	} {
		t.Run(tc.line, func(t *testing.T) {
			if tc.line == "" {
				return // splitParagraphs never passes an empty line
			}
			if got := startsSubArticle(tc.line); got != tc.want {
				t.Errorf("startsSubArticle(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
