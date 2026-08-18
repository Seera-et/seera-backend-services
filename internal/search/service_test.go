package search

import (
	"strings"
	"testing"
)

func TestParseArticleRefs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "english article",
			query: "What does Article 45 say about formation?",
			want:  []string{"45"},
		},
		{
			name:  "abbreviated",
			query: "See Art. 12 and art 13",
			want:  []string{"12", "13"},
		},
		{
			name:  "with sub-article",
			query: "Article 27(2) of the labour proclamation",
			want:  []string{"27"},
		},
		{
			name:  "amharic with ge'ez numeral",
			query: "አንቀጽ ፵፭ ስለ ምን ይናገራል?",
			want:  []string{"45"},
		},
		{
			name:  "amharic with arabic numeral",
			query: "አንቀጽ 45",
			want:  []string{"45"},
		},
		{
			name:  "plural",
			query: "Compare articles 45 and 46",
			want:  []string{"45", "46"},
		},
		{
			name:  "lettered article",
			query: "Article 45a was inserted by the amendment",
			want:  []string{"45a"},
		},
		{
			name:  "enumeration with commas",
			query: "See arts. 12, 13 and 14",
			want:  []string{"12", "13", "14"},
		},
		{
			name:  "amharic enumeration",
			query: "አንቀጽ ፵፭ እና ፵፮",
			want:  []string{"45", "46"},
		},
		{
			name:  "no reference",
			query: "What are the requirements for forming a private limited company?",
			want:  nil,
		},
		{
			// "and" only continues an enumeration when a number follows it.
			name:  "trailing and is not a reference",
			query: "Article 45 and the company registration process",
			want:  []string{"45"},
		},
		{
			name:  "duplicate references collapse",
			query: "Article 45 ... as stated in Article 45",
			want:  []string{"45"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := ParseArticleRefs(tc.query)

			got := make([]string, len(refs))
			for i, r := range refs {
				got[i] = r.Number
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ParseArticleRefs(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// This mirrors workers/extraction/test_extraction.py. The two implementations
// normalise the same numerals on opposite sides of the pipeline — Python at
// ingest, Go at query time — and a disagreement between them means a query for
// አንቀጽ ፵፭ silently fails to find the article stored as "45".
func TestGeezToInt(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
		ok   bool
	}{
		{"፩", 1, true},
		{"፱", 9, true},
		{"፲", 10, true},
		{"፲፪", 12, true},
		{"፺፱", 99, true},
		{"፻", 100, true},
		{"፪፻", 200, true},
		{"፪፻፴፬", 234, true},
		{"፱፻፺፱", 999, true},
		{"፼", 10000, true},
		{"፵፭", 45, true},
		{"", 0, false},
		{"45", 0, false},
		{"አንቀጽ", 0, false},
		{"፵a", 0, false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, ok := GeezToInt(tc.text)
			if ok != tc.ok {
				t.Fatalf("GeezToInt(%q) ok = %v, want %v", tc.text, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("GeezToInt(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestNormalizeNumber(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"፵፭", "45"},
		{"45", "45"},
		{"45A", "45a"},
		{" ፲፪ ", "12"},
		{"", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := NormalizeNumber(tc.raw); got != tc.want {
				t.Errorf("NormalizeNumber(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestQueryDefaults(t *testing.T) {
	q := Query{Text: "anything"}.withDefaults()

	if q.PerRetriever != defaultPerRetriever {
		t.Errorf("PerRetriever = %d, want %d", q.PerRetriever, defaultPerRetriever)
	}
	if q.Limit != defaultLimit {
		t.Errorf("Limit = %d, want %d", q.Limit, defaultLimit)
	}
	if q.AsOf.IsZero() {
		t.Error("AsOf was left zero; the in-force filter would exclude every provision")
	}

	// Explicit values are not overwritten.
	explicit := Query{Text: "x", PerRetriever: 10, Limit: 5}.withDefaults()
	if explicit.PerRetriever != 10 || explicit.Limit != 5 {
		t.Errorf("defaults overwrote explicit values: %+v", explicit)
	}
}
