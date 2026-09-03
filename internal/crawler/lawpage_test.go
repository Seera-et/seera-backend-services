package crawler

import (
	"os"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// The fixture is a verbatim capture of a live Ministry law page, kept whole
// rather than trimmed. Its job is to fail when the site is redesigned, and a
// fixture edited down to the parts that currently matter cannot do that.
func realLawPage(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/federal-courts-proclamation.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

func TestParseRealLawPage(t *testing.T) {
	ref := LawPageRef{
		URL:      lawPage,
		Slug:     "federal-courts-procla",
		Language: "en",
	}

	page, err := ParseLawPage(ref, realLawPage(t))
	if err != nil {
		t.Fatalf("ParseLawPage: %v", err)
	}

	t.Run("bilingual title is split", func(t *testing.T) {
		if !strings.Contains(page.TitleEn, "Federal Courts Proclamation") {
			t.Errorf("English title = %q", page.TitleEn)
		}
		if !strings.Contains(page.TitleAm, "የፌዴራል ፍርድ ቤቶች") {
			t.Errorf("Amharic title = %q", page.TitleAm)
		}
		if strings.Contains(page.TitleEn, "Ministry of Justice") {
			t.Errorf("English title kept the site suffix: %q", page.TitleEn)
		}
	})

	t.Run("law id and type", func(t *testing.T) {
		if page.DocumentNumber != "1234/2021" {
			t.Errorf("document number = %q, want %q", page.DocumentNumber, "1234/2021")
		}
		if page.DocType != legal.DocTypeProclamation {
			t.Errorf("doc type = %q, want %q", page.DocType, legal.DocTypeProclamation)
		}
	})

	t.Run("publication date", func(t *testing.T) {
		if page.PublicationDate == nil {
			t.Fatal("no publication date parsed from a page that states one")
		}
		if got := page.PublicationDate.Format("2006-01-02"); got != "2021-04-26" {
			t.Errorf("publication date = %s, want 2021-04-26", got)
		}
	})

	t.Run("status", func(t *testing.T) {
		if page.Status != "In Force" {
			t.Errorf("status = %q, want %q", page.Status, "In Force")
		}
	})

	// The whole point of the resolver, on a real page: the English page links a
	// PDF whose filename is Amharic, and the link is the jepe download button.
	t.Run("the real PDF is found", func(t *testing.T) {
		if len(page.Candidates) == 0 {
			t.Fatal("no PDF candidates on a page that has a download button")
		}
		best := page.Candidates[0]
		if best.Source != SourceDownloadButton {
			t.Errorf("best candidate came from %q, want the download button", best.Source)
		}
		if !strings.HasPrefix(best.URL, "https://justice.gov.et/wp-content/uploads/") {
			t.Errorf("best candidate URL = %q", best.URL)
		}
		if !strings.Contains(best.Display, "አዋጅ") {
			t.Errorf("display URL lost the Amharic filename: %q", best.Display)
		}
		if strings.Contains(best.URL, "jet_download") {
			t.Error("selected the JetElements endpoint over the real link")
		}
	})
}

func TestSplitBilingualTitle(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		wantAm string
		wantEn string
	}{
		{
			name:   "both languages",
			title:  "የፌዴራል ፍርድ ቤቶች አዋጅ • Federal Courts Proclamation - Ministry of Justice",
			wantAm: "የፌዴራል ፍርድ ቤቶች አዋጅ",
			wantEn: "Federal Courts Proclamation",
		},
		{
			name:   "english only",
			title:  "Federal Courts Proclamation - Ministry of Justice",
			wantAm: "",
			wantEn: "Federal Courts Proclamation",
		},
		{
			name:   "amharic only is not filed as english",
			title:  "የፌዴራል ፍርድ ቤቶች አዋጅ - Ministry of Justice",
			wantAm: "የፌዴራል ፍርድ ቤቶች አዋጅ",
			wantEn: "",
		},
		{name: "empty", title: "", wantAm: "", wantEn: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			am, en := splitBilingualTitle(tt.title)
			if am != tt.wantAm {
				t.Errorf("amharic = %q, want %q", am, tt.wantAm)
			}
			if en != tt.wantEn {
				t.Errorf("english = %q, want %q", en, tt.wantEn)
			}
		})
	}
}

func TestNumberYear(t *testing.T) {
	tests := []struct {
		number string
		want   int
		ok     bool
	}{
		{"1234/2021", 2021, true},
		{"1234/2013", 2013, true},
		{"179/1998", 1998, true},
		{"", 0, false},
		{"not-a-number", 0, false},
	}
	for _, tt := range tests {
		got, ok := NumberYear(tt.number)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NumberYear(%q) = (%d, %v), want (%d, %v)", tt.number, got, ok, tt.want, tt.ok)
		}
	}
}

// The same instrument is 1234/2013 in Amharic and 1234/2021 in English, so the
// calendar follows the page, not the number.
//
// No year-value heuristic can do this: Proclamation 179 is 179/1991 E.C. and
// 179/1998 G.C., and both are plausible four-digit years. Reading the Ethiopian
// year as Gregorian would date the law eight years early and change which
// questions the in-force filter answers with it.
func TestCalendarFollowsThePageNotTheNumber(t *testing.T) {
	if !IsEthiopianCalendar("am") {
		t.Error("an Amharic page prints Ethiopian years")
	}
	if IsEthiopianCalendar("en") {
		t.Error("an English page prints Gregorian years")
	}

	// The historical pair no threshold could separate.
	if _, ok := GregorianYear("179/1991", "am"); ok {
		t.Error("claimed to know the Gregorian year of an Ethiopian-calendar number")
	}
	got, ok := GregorianYear("179/1998", "en")
	if !ok || got != 1998 {
		t.Errorf("GregorianYear(179/1998, en) = (%d, %v), want (1998, true)", got, ok)
	}
}

// Not every law page renders a "Law ID:" field. The loan-ratification
// proclamations do not, and without a title fallback they would be stored with
// no document number at all — unciteable.
func TestNumberFromTitle(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		wantType   string
		wantNumber string
		wantOK     bool
	}{
		{
			name: "type word appears twice, the numbered one wins",
			title: "A PROCLAMATION TO RATIFY THE LOAN AGREEMENT BETWEEN THE FEDERAL DEMOCRATIC " +
				"REPUBLIC OF ETHIOPIA AND EXPORT-IMPORT BANK OF KOREA PROCLAMATION No. 1407/2026",
			wantType: "PROCLAMATION", wantNumber: "1407/2026", wantOK: true,
		},
		{
			name:     "parenthesised form",
			title:    "Federal Courts Proclamation (Proclamation No. 1234/2021)",
			wantType: "Proclamation", wantNumber: "1234/2021", wantOK: true,
		},
		{
			name:     "regulation",
			title:    "Some Regulation No. 555/2023",
			wantType: "Regulation", wantNumber: "555/2023", wantOK: true,
		},
		{name: "no number", title: "A Proclamation about things", wantOK: false},
		{name: "empty", title: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docType, number, ok := numberFromTitle(tt.title)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if docType != tt.wantType || number != tt.wantNumber {
				t.Errorf("got (%q, %q), want (%q, %q)", docType, number, tt.wantType, tt.wantNumber)
			}
		})
	}
}

func TestDocTypeFrom(t *testing.T) {
	tests := []struct {
		raw  string
		want legal.DocType
	}{
		{"Proclamation", legal.DocTypeProclamation},
		{"regulation", legal.DocTypeRegulation},
		{" Directive ", legal.DocTypeDirective},
		{"Code", legal.DocTypeCode},
		{"Something Else", legal.DocTypeOther},
		{"", legal.DocTypeOther},
	}
	for _, tt := range tests {
		if got := docTypeFrom(tt.raw); got != tt.want {
			t.Errorf("docTypeFrom(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

// A page that states nothing must yield nothing, not defaults that look like
// facts.
func TestMissingMetadataStaysEmpty(t *testing.T) {
	page, err := ParseLawPage(LawPageRef{URL: lawPage}, []byte(`<html><body><p>Nothing here.</p></body></html>`))
	if err != nil {
		t.Fatalf("ParseLawPage: %v", err)
	}
	if page.DocumentNumber != "" || page.Status != "" || page.DocTypeRaw != "" {
		t.Errorf("invented metadata: %+v", page)
	}
	if page.PublicationDate != nil {
		t.Errorf("invented a publication date: %v", page.PublicationDate)
	}
}
