package crawler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// BaseURL is the Ministry of Justice site this crawler is written against.
//
// Source-specific by design. The extraction and ingestion stages downstream are
// reusable across Ethiopian legal sources; this stage is not, because every
// site's markup and pagination differ, and pretending otherwise produces a
// configurable crawler that works properly on none of them.
const BaseURL = "https://justice.gov.et"

// DefaultIndexPaths are the listings walked when none are configured.
var DefaultIndexPaths = []string{
	"/en/laws/proclamations/",
	"/en/laws/regulations/",
	"/en/registered-directives/",
}

// lawPathPattern matches a law page path: /en/law/<slug>/ or /am/law/<slug>/.
//
// Anchored, so a link merely containing "/law/" — a listing filter, a tag page
// — is not mistaken for a document.
var lawPathPattern = regexp.MustCompile(`^/(en|am)/law/([^/]+)/?$`)

// pagePathPattern matches WordPress pagination: .../page/2/.
var pagePathPattern = regexp.MustCompile(`/page/(\d+)/?$`)

// LawPageRef is a discovered law page, before it has been fetched.
type LawPageRef struct {
	// URL is normalised and absolute.
	URL string `json:"url"`

	// Slug identifies the law, and is shared by its English and Amharic pages.
	Slug string `json:"slug"`

	// Language is the language of the *page*, not of the document it links.
	// The Ministry's English pages routinely link Amharic PDFs, so this is
	// provenance, never a claim about the file — see LawPage.
	Language string `json:"page_language"`

	// Title is the listing's link text, when it had any. A better title is
	// taken from the law page itself later.
	Title string `json:"title,omitempty"`

	DiscoveredAt time.Time `json:"discovered_at"`
}

// DiscoverOptions bounds a discovery run.
type DiscoverOptions struct {
	// IndexPaths are the listings to walk. Empty means DefaultIndexPaths.
	IndexPaths []string

	// MaxPages caps pagination per listing. Zero means no cap.
	MaxPages int

	// Limit stops discovery once this many law pages are found. Zero means no
	// limit. The first real run against the Ministry should always set one.
	Limit int
}

// Discoverer walks the Ministry's listings and reports the law pages on them.
type Discoverer struct {
	client *Client
	base   *url.URL
	log    *slog.Logger
}

func NewDiscoverer(client *Client, log *slog.Logger) (*Discoverer, error) {
	base, err := url.Parse(BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Discoverer{client: client, base: base, log: log}, nil
}

// Discover walks every configured listing and returns the law pages found,
// deduplicated by normalised URL and ordered by slug for a stable run-to-run
// report.
//
// A listing that fails is logged and skipped rather than failing the run: the
// Ministry's three listings are independent, and losing directives is not a
// reason to lose proclamations.
func (d *Discoverer) Discover(ctx context.Context, opts DiscoverOptions) ([]LawPageRef, error) {
	paths := opts.IndexPaths
	if len(paths) == 0 {
		paths = DefaultIndexPaths
	}

	found := map[string]LawPageRef{}
	for _, path := range paths {
		if opts.Limit > 0 && len(found) >= opts.Limit {
			break
		}
		if err := d.walkListing(ctx, path, opts, found); err != nil {
			d.log.WarnContext(ctx, "listing walk failed, continuing with the others",
				slog.String("path", path), slog.Any("error", err))
		}
	}

	out := make([]LawPageRef, 0, len(found))
	for _, ref := range found {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })

	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// walkListing pages through one listing, adding what it finds to found.
//
// It stops when a page yields no law page it has not already seen. That is a
// more reliable end condition than trusting the pagination widget, which on
// this site renders a truncated range rather than the true last page.
func (d *Discoverer) walkListing(ctx context.Context, path string, opts DiscoverOptions, found map[string]LawPageRef) error {
	highest := 1

	for page := 1; ; page++ {
		if opts.MaxPages > 0 && page > opts.MaxPages {
			return nil
		}
		if opts.Limit > 0 && len(found) >= opts.Limit {
			return nil
		}

		pageURL := d.listingURL(path, page)
		doc, err := d.fetchHTML(ctx, pageURL)
		if err != nil {
			if page == 1 {
				return err
			}
			// Paging past the end is an expected way to finish, not a fault.
			d.log.DebugContext(ctx, "listing page unavailable, assuming end of listing",
				slog.String("url", pageURL), slog.Any("error", err))
			return nil
		}

		refs, maxPage := d.parseListing(doc, pageURL)
		if maxPage > highest {
			highest = maxPage
		}

		var added int
		for _, ref := range refs {
			if _, seen := found[ref.URL]; seen {
				continue
			}
			found[ref.URL] = ref
			added++
			d.log.InfoContext(ctx, "DISCOVERED",
				slog.String("url", ref.URL),
				slog.String("slug", ref.Slug))
			if opts.Limit > 0 && len(found) >= opts.Limit {
				return nil
			}
		}

		d.log.DebugContext(ctx, "listing page walked",
			slog.String("url", pageURL),
			slog.Int("links", len(refs)),
			slog.Int("new", added),
			slog.Int("total", len(found)))

		if added == 0 && page >= highest {
			return nil
		}
	}
}

// listingURL builds the URL of one page of a listing.
func (d *Discoverer) listingURL(path string, page int) string {
	u := *d.base
	u.Path = path
	if page > 1 {
		u.Path = strings.TrimSuffix(path, "/") + "/page/" + strconv.Itoa(page) + "/"
	}
	return u.String()
}

// parseListing pulls law page links and the highest pagination number out of
// one listing page.
func (d *Discoverer) parseListing(doc *html.Node, pageURL string) ([]LawPageRef, int) {
	base, err := url.Parse(pageURL)
	if err != nil {
		base = d.base
	}

	var (
		refs    []LawPageRef
		seen    = map[string]bool{}
		maxPage = 1
		now     = time.Now().UTC()
	)

	for _, a := range elementsByTag(doc, "a") {
		href, ok := attr(a, "href")
		if !ok {
			continue
		}
		resolved, err := base.Parse(strings.TrimSpace(href))
		if err != nil || resolved.Host != d.base.Host {
			continue
		}

		if m := pagePathPattern.FindStringSubmatch(resolved.Path); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxPage {
				maxPage = n
			}
			continue
		}

		m := lawPathPattern.FindStringSubmatch(resolved.Path)
		if m == nil {
			continue
		}

		// Normalised: no query, no fragment, always trailing-slashed, so the
		// same law linked from a listing, a tag page and a search result
		// collapses to one entry.
		normalised := *resolved
		normalised.RawQuery = ""
		normalised.Fragment = ""
		normalised.Path = strings.TrimSuffix(resolved.Path, "/") + "/"
		key := normalised.String()

		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, LawPageRef{
			URL:          key,
			Slug:         m[2],
			Language:     m[1],
			Title:        textOf(a),
			DiscoveredAt: now,
		})
	}

	return refs, maxPage
}

// fetchHTML gets a page and parses it.
func (d *Discoverer) fetchHTML(ctx context.Context, rawURL string) (*html.Node, error) {
	resp, err := d.client.Get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// Bounded: a listing page is tens of kilobytes, and an unbounded read here
	// would let a misrouted response consume memory without limit.
	body := io.LimitReader(resp.Body, 8<<20)
	doc, err := html.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rawURL, err)
	}
	return doc, nil
}
