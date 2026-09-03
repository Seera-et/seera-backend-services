package crawler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/Seera-et/seera-backend-services/internal/lang"
	"github.com/Seera-et/seera-backend-services/internal/legal"
)

// LawPage is one law's page, parsed.
//
// Everything here comes from the listing page's own markup. Fields the page did
// not state are left zero — nothing is inferred, because a guessed effective
// date or document number produces a citation that looks authoritative and is
// wrong.
type LawPage struct {
	Ref LawPageRef `json:"ref"`

	// TitleAm and TitleEn are the two halves of the page's bilingual title.
	// Either may be empty on a page that only carries one.
	TitleAm string `json:"title_am,omitempty"`
	TitleEn string `json:"title_en,omitempty"`

	// DocumentNumber is the identifier as printed, e.g. "1234/2021". The year
	// in it follows the page's own calendar — see YearIsEthiopian.
	DocumentNumber string `json:"document_number,omitempty"`

	// DocTypeRaw is the word the page used ("Proclamation", "Regulation").
	DocTypeRaw string        `json:"doc_type_raw,omitempty"`
	DocType    legal.DocType `json:"doc_type,omitempty"`

	// PublicationDate is the date the page printed, in Gregorian terms. Nil
	// when the page did not state one or it could not be parsed.
	PublicationDate *time.Time `json:"publication_date,omitempty"`

	// Status is the Ministry's own label: "In Force", "Repealed", "Amended".
	Status string `json:"status,omitempty"`

	// Candidates are the PDF links found, best first.
	Candidates []Candidate `json:"candidates,omitempty"`

	FetchedAt time.Time `json:"fetched_at"`
}

// titleSeparator splits the Ministry's bilingual page titles:
//
//	የፌዴራል ፍርድ ቤቶች አዋጅ (አዋጅ ቁጥር 1234/2013) • Federal Courts Proclamation (Proclamation No. 1234/2021) - Ministry of Justice
//
// Note the two numbers. The Amharic half dates in the Ethiopian calendar and
// the English half in the Gregorian one, for the same instrument.
const titleSeparator = "•"

const titleSuffix = " - Ministry of Justice"

var (
	// "Law ID: Proclamation No. 1234/2021"
	lawIDPattern = regexp.MustCompile(
		`(?i)Law\s*ID\s*:\s*([A-Za-z ]+?)\s*No\.?\s*([0-9]+\s*/\s*[0-9]+)`)

	// "Date: April 26, 2021"
	datePattern = regexp.MustCompile(`(?i)Date\s*:\s*(.+)$`)

	// The Ministry's three status labels, English and Amharic.
	statusPattern = regexp.MustCompile(
		`^(In Force|Repealed|Amended|በአገልግሎት ላይ|የተሻረ|የተሻሻለ)$`)

	// The year half of a document number.
	numberYearPattern = regexp.MustCompile(`/\s*([0-9]{2,4})\s*$`)
)

// dateLayouts are the forms the Ministry's "Date:" field has been seen in.
// Parsing is strict: an unrecognised date leaves the field nil rather than
// being coerced into a plausible one.
var dateLayouts = []string{
	"January 2, 2006",
	"2 January 2006",
	"2006-01-02",
	"02/01/2006",
}

// Fetcher retrieves and parses individual law pages.
type Fetcher struct {
	client *Client
	log    *slog.Logger
}

func NewFetcher(client *Client, log *slog.Logger) *Fetcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Fetcher{client: client, log: log}
}

// Fetch retrieves one law page and parses everything it states.
func (f *Fetcher) Fetch(ctx context.Context, ref LawPageRef) (LawPage, error) {
	resp, err := f.client.Get(ctx, ref.URL)
	if err != nil {
		return LawPage{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return LawPage{}, fmt.Errorf("read law page %s: %w", ref.URL, err)
	}

	page, err := ParseLawPage(ref, body)
	if err != nil {
		return LawPage{}, err
	}

	f.log.InfoContext(ctx, "LAW_PAGE_FETCHED",
		slog.String("url", ref.URL),
		slog.String("number", page.DocumentNumber),
		slog.String("status", page.Status),
		slog.Int("candidates", len(page.Candidates)))

	return page, nil
}

// ParseLawPage extracts metadata and PDF candidates from a law page's HTML.
//
// Separated from fetching so the whole parse is testable against fixtures, and
// so a site redesign is diagnosed by running this over a saved page rather than
// by re-crawling.
//
// The Ministry's pages are Elementor/JetEngine output: the element classes are
// generated (`elementor-element-8b94256`) and change when the page is edited,
// so nothing here keys on them. The metadata is read from the *text* of the
// dynamic fields, which is label-prefixed ("Law ID: …", "Date: …") and has
// proven stable.
func ParseLawPage(ref LawPageRef, body []byte) (LawPage, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return LawPage{}, fmt.Errorf("parse law page %s: %w", ref.URL, err)
	}

	page := LawPage{Ref: ref, FetchedAt: time.Now().UTC()}

	if titles := elementsByTag(doc, "title"); len(titles) > 0 {
		page.TitleAm, page.TitleEn = splitBilingualTitle(textOf(titles[0]))
	}

	// Each field is tested against every pattern, and the first value found for
	// a given field wins. The page renders the same value into several
	// responsive variants, so later repeats carry no new information.
	for _, field := range dynamicFieldTexts(doc) {
		if m := lawIDPattern.FindStringSubmatch(field); m != nil && page.DocumentNumber == "" {
			page.DocTypeRaw = strings.TrimSpace(m[1])
			page.DocType = docTypeFrom(page.DocTypeRaw)
			page.DocumentNumber = strings.ReplaceAll(strings.TrimSpace(m[2]), " ", "")
			continue
		}
		if m := datePattern.FindStringSubmatch(field); m != nil && page.PublicationDate == nil {
			if t, ok := parseDate(strings.TrimSpace(m[1])); ok {
				page.PublicationDate = &t
			}
			continue
		}
		if page.Status == "" && statusPattern.MatchString(field) {
			page.Status = field
		}
	}

	// Not every law page renders a "Law ID:" field — the loan-ratification
	// proclamations do not — but the title carries the number on every page
	// seen so far ("… PROCLAMATION No. 1407/2026"). Read from the English half,
	// which prints the Gregorian year; the Amharic half of the same title says
	// 1407/2018.
	if page.DocumentNumber == "" {
		if docType, number, ok := numberFromTitle(page.TitleEn); ok {
			page.DocTypeRaw = docType
			page.DocType = docTypeFrom(docType)
			page.DocumentNumber = number
		}
	}

	candidates, err := PDFCandidates(ref.URL, body)
	if err != nil {
		return LawPage{}, err
	}
	page.Candidates = candidates

	return page, nil
}

// titleNumberPattern reads the instrument and number out of a title:
//
//	"… EXPORT-IMPORT BANK OF KOREA PROCLAMATION No. 1407/2026"
//	"Federal Courts Proclamation (Proclamation No. 1234/2021)"
//
// The last match wins, because the type word appears in the descriptive part of
// a title too ("A PROCLAMATION TO RATIFY …") and only the trailing one is
// followed by the number.
var titleNumberPattern = regexp.MustCompile(
	`(?i)\b(Proclamation|Regulation|Directive|Code)s?\b[\s.]*No\.?\s*([0-9]+\s*/\s*[0-9]+)`)

// numberFromTitle recovers the document type and number from a page title.
func numberFromTitle(title string) (docType, number string, ok bool) {
	matches := titleNumberPattern.FindAllStringSubmatch(title, -1)
	if len(matches) == 0 {
		return "", "", false
	}
	last := matches[len(matches)-1]
	return strings.TrimSpace(last[1]), strings.ReplaceAll(strings.TrimSpace(last[2]), " ", ""), true
}

// dynamicFieldTexts collects the text of every JetEngine dynamic field on the
// page, deduplicated — the Ministry's templates render the same value into
// several responsive variants of the same block.
func dynamicFieldTexts(doc *html.Node) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	walk(doc, func(n *html.Node) {
		if !hasClass(n, "jet-listing-dynamic-field__content") {
			return
		}
		text := textOf(n)
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		out = append(out, text)
	})
	return out
}

// splitBilingualTitle separates the Amharic and English halves of a page title.
//
// A page carrying only one language returns it as the English half when it has
// no Ethiopic script, and as the Amharic half when it does — better than
// silently filing an Amharic title in an English field.
func splitBilingualTitle(title string) (am, en string) {
	title = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), titleSuffix))
	if title == "" {
		return "", ""
	}

	if before, after, found := strings.Cut(title, titleSeparator); found {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	if lang.HasEthiopicScript(title) {
		return title, ""
	}
	return "", title
}

// docTypeFrom maps the Ministry's own word onto the domain's enum. Anything
// unrecognised becomes DocTypeOther rather than being forced into a category
// it may not belong to.
func docTypeFrom(raw string) legal.DocType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "proclamation":
		return legal.DocTypeProclamation
	case "regulation":
		return legal.DocTypeRegulation
	case "directive":
		return legal.DocTypeDirective
	case "code":
		return legal.DocTypeCode
	default:
		return legal.DocTypeOther
	}
}

// parseDate tries the known layouts and gives up rather than guessing.
func parseDate(s string) (time.Time, bool) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// NumberYear extracts the year from a document number ("1234/2021" → 2021).
func NumberYear(documentNumber string) (int, bool) {
	m := numberYearPattern.FindStringSubmatch(documentNumber)
	if m == nil {
		return 0, false
	}
	year, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return year, true
}

// IsEthiopianCalendar reports whether a year printed on a page of the given
// language is in the Ethiopian calendar.
//
// The Ministry prints the same instrument two ways: Proclamation 1234 is
// "1234/2013" on its Amharic page and "1234/2021" on its English one. The
// calendar follows the page, not the number.
//
// There is deliberately no year-value heuristic here. An earlier attempt used
// one and it was wrong: Proclamation 179 is 179/1991 E.C. and 179/1998 G.C.,
// so for historical laws both readings are perfectly plausible four-digit
// years and no threshold separates them. The page's language does.
func IsEthiopianCalendar(pageLanguage string) bool {
	return strings.EqualFold(pageLanguage, "am")
}

// GregorianYear returns a document number's year in the Gregorian calendar,
// and whether it is known.
//
// Known only when the number came from an English page, which already prints
// Gregorian. An Ethiopian-calendar year is *not* converted: the E.C.→G.C.
// offset is 7 or 8 depending on where in the year the date falls, the English
// page states the Gregorian form anyway, and a date guessed wrong by a year
// silently changes which law the in-force SQL filters return. Crawl the English
// page for the Gregorian year, or leave the field null.
func GregorianYear(documentNumber, pageLanguage string) (int, bool) {
	if IsEthiopianCalendar(pageLanguage) {
		return 0, false
	}
	return NumberYear(documentNumber)
}
