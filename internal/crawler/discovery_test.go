package crawler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// discovererFor points a Discoverer at a test server instead of the Ministry.
func discovererFor(t *testing.T, serverURL string) *Discoverer {
	t.Helper()
	base, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	return &Discoverer{
		client: NewClient(testClientConfig(), nil),
		base:   base,
		log:    discardLogger(),
	}
}

func parseHTML(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse html: %v", err)
	}
	return doc
}

func TestParseListingFindsLawPagesAndIgnoresEverythingElse(t *testing.T) {
	d := discovererFor(t, "https://justice.gov.et")

	page := `<html><body>
	  <a href="/en/law/federal-courts-procla/">Federal Courts Proclamation</a>
	  <a href="/en/law/ethiopian-building-proclamation/">Building Proclamation</a>
	  <a href="/am/law/federal-courts-procla/">አማርኛ</a>
	  <a href="/en/laws/proclamations/">Back to proclamations</a>
	  <a href="/en/tags/proclamation-no-354-2004/">tag page</a>
	  <a href="/en/resources/">Resources</a>
	  <a href="https://example.com/en/law/not-ours/">Another site</a>
	</body></html>`

	refs, _ := d.parseListing(parseHTML(t, page), "https://justice.gov.et/en/laws/proclamations/")

	got := map[string]string{}
	for _, r := range refs {
		got[r.URL] = r.Language
	}
	want := map[string]string{
		"https://justice.gov.et/en/law/federal-courts-procla/":           "en",
		"https://justice.gov.et/en/law/ethiopian-building-proclamation/": "en",
		"https://justice.gov.et/am/law/federal-courts-procla/":           "am",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d law pages, want %d: %v", len(got), len(want), got)
	}
	for url, lang := range want {
		if got[url] != lang {
			t.Errorf("%s: language = %q, want %q", url, got[url], lang)
		}
	}
}

// A law reachable from a listing, a tag page and a search result is one law.
func TestListingURLsAreNormalisedAndDeduped(t *testing.T) {
	d := discovererFor(t, "https://justice.gov.et")

	page := `<html><body>
	  <a href="/en/law/federal-courts-procla/">A</a>
	  <a href="/en/law/federal-courts-procla">B, no trailing slash</a>
	  <a href="/en/law/federal-courts-procla/?utm_source=x">C, tracking query</a>
	  <a href="/en/law/federal-courts-procla/#section">D, fragment</a>
	  <a href="https://justice.gov.et/en/law/federal-courts-procla/">E, absolute</a>
	</body></html>`

	refs, _ := d.parseListing(parseHTML(t, page), "https://justice.gov.et/en/laws/proclamations/")

	if len(refs) != 1 {
		urls := make([]string, len(refs))
		for i, r := range refs {
			urls[i] = r.URL
		}
		t.Fatalf("got %d refs for one law: %v", len(refs), urls)
	}
	if refs[0].URL != "https://justice.gov.et/en/law/federal-courts-procla/" {
		t.Errorf("normalised URL = %q", refs[0].URL)
	}
	if refs[0].Slug != "federal-courts-procla" {
		t.Errorf("slug = %q", refs[0].Slug)
	}
}

func TestParseListingReadsPagination(t *testing.T) {
	d := discovererFor(t, "https://justice.gov.et")

	page := `<html><body>
	  <a href="/en/law/one/">One</a>
	  <a href="/en/laws/proclamations/page/2/">2</a>
	  <a href="/en/laws/proclamations/page/7/">7</a>
	  <a href="/en/laws/proclamations/page/3/">3</a>
	</body></html>`

	refs, maxPage := d.parseListing(parseHTML(t, page), "https://justice.gov.et/en/laws/proclamations/")

	if maxPage != 7 {
		t.Errorf("highest page = %d, want 7", maxPage)
	}
	if len(refs) != 1 {
		t.Errorf("pagination links were counted as law pages: %v", refs)
	}
}

// Walking must follow pagination and stop on its own — the Ministry's paginator
// renders a truncated range, so the end condition is "this page told me nothing
// new", not "the widget says we are done".
func TestDiscoverWalksPaginationAndStops(t *testing.T) {
	const pages = 3
	var served int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		page := 1
		if m := pagePathPattern.FindStringSubmatch(r.URL.Path); m != nil {
			_, _ = fmt.Sscanf(m[1], "%d", &page)
		}
		if page > pages {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<html><body>
		  <a href="/en/law/law-%d-a/">A</a>
		  <a href="/en/law/law-%d-b/">B</a>
		  <a href="/en/laws/proclamations/page/2/">2</a>
		  <a href="/en/laws/proclamations/page/3/">3</a>
		</body></html>`, page, page)
	}))
	defer srv.Close()

	refs, err := discovererFor(t, srv.URL).Discover(context.Background(), DiscoverOptions{
		IndexPaths: []string{"/en/laws/proclamations/"},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(refs) != pages*2 {
		t.Errorf("found %d law pages, want %d", len(refs), pages*2)
	}
	if served < pages {
		t.Errorf("fetched %d listing pages, want at least %d", served, pages)
	}
}

func TestDiscoverRespectsLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		for i := range 50 {
			fmt.Fprintf(&b, `<a href="/en/law/law-%02d/">L</a>`, i)
		}
		fmt.Fprintf(w, "<html><body>%s</body></html>", b.String())
	}))
	defer srv.Close()

	refs, err := discovererFor(t, srv.URL).Discover(context.Background(), DiscoverOptions{
		IndexPaths: []string{"/en/laws/proclamations/"},
		Limit:      7,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(refs) != 7 {
		t.Errorf("got %d law pages, want the limit of 7", len(refs))
	}
}

// Three independent listings: losing directives is not a reason to lose
// proclamations.
func TestDiscoverContinuesWhenOneListingFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "registered-directives") {
			http.Error(w, "gone", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`<html><body><a href="/en/law/survivor/">S</a></body></html>`))
	}))
	defer srv.Close()

	refs, err := discovererFor(t, srv.URL).Discover(context.Background(), DiscoverOptions{
		IndexPaths: []string{"/en/registered-directives/", "/en/laws/proclamations/"},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(refs) != 1 || refs[0].Slug != "survivor" {
		t.Errorf("got %v, want the proclamation listing's one law page", refs)
	}
}

func TestListingURL(t *testing.T) {
	d := discovererFor(t, "https://justice.gov.et")

	tests := []struct {
		page int
		want string
	}{
		{1, "https://justice.gov.et/en/laws/proclamations/"},
		{2, "https://justice.gov.et/en/laws/proclamations/page/2/"},
		{17, "https://justice.gov.et/en/laws/proclamations/page/17/"},
	}
	for _, tt := range tests {
		if got := d.listingURL("/en/laws/proclamations/", tt.page); got != tt.want {
			t.Errorf("listingURL(page %d) = %q, want %q", tt.page, got, tt.want)
		}
	}
}
