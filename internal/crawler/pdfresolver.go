// Package crawler acquires Ethiopian legal documents from justice.gov.et:
// discover law pages, find the real PDF behind each one, download it, and prove
// it is a PDF before anything downstream treats it as one.
//
// It stops there. Text extraction, OCR and structure detection happen offline in
// workers/extraction, whose JSON internal/ingestion already consumes — see that
// package's doc comment for the contract.
package crawler

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// CandidateSource says which piece of markup a PDF URL came from. It decides
// priority, and it is logged: when the wrong file is downloaded, the first
// question is which link won.
type CandidateSource string

const (
	// SourceDownloadButton is a.jepe-download-button — the Ministry's real
	// download link, and the only one that reliably points at the document.
	SourceDownloadButton CandidateSource = "jepe-download-button"

	// SourceDirectPDF is any anchor whose href already ends in .pdf, usually
	// under /wp-content/uploads/.
	SourceDirectPDF CandidateSource = "direct-pdf"

	// SourceEmbedded is an iframe/object/embed pointing at a PDF.
	SourceEmbedded CandidateSource = "embedded"

	// SourceJetDownload is a.jet-download — the Elementor/JetElements endpoint,
	// `?jet_download=<hash>`. Kept only so a page with nothing else still has
	// something to probe; it returns HTTP 500 often enough that it is never
	// trusted without being fetched first. See Candidate.Trusted.
	SourceJetDownload CandidateSource = "jet-download"
)

// priority orders the sources. Lower wins.
func (s CandidateSource) priority() int {
	switch s {
	case SourceDownloadButton:
		return 0
	case SourceDirectPDF:
		return 1
	case SourceEmbedded:
		return 2
	default:
		return 3
	}
}

// Candidate is one possible PDF URL found on a law page.
type Candidate struct {
	// URL is request-ready: absolute, and percent-encoded as the server
	// expects. Never a chrome-extension:// wrapper.
	URL string `json:"url"`

	// Display is the same URL with its path decoded, for logs and for humans
	// reading an Amharic filename. Never used to make a request.
	Display string `json:"display,omitempty"`

	Source CandidateSource `json:"source"`

	// Label is the link's visible text, when it had any.
	Label string `json:"label,omitempty"`
}

// Trusted reports whether a candidate may be downloaded on the strength of its
// markup alone.
//
// Only the JetElements endpoint is untrusted. It is a query parameter on the
// site root, not a file path — nothing about it says "PDF", and in practice it
// answers 500 while the real file sits in another element on the same page.
// Everything else still has to pass content validation; this only says whether
// a failure is expected enough to keep looking rather than to give up.
func (c Candidate) Trusted() bool { return c.Source != SourceJetDownload }

// IsJetDownload reports whether a URL is the JetElements download endpoint.
func IsJetDownload(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Query().Has("jet_download")
}

// chromeViewerPrefix is what Chrome's bundled PDF viewer prepends to a URL when
// someone copies it out of the address bar. It is not a location this or any
// other program can fetch, and it must never be stored.
const chromeViewerPrefix = "chrome-extension://"

// stripChromeViewer unwraps a Chrome PDF viewer URL, returning the real URL
// inside it. Anything else is returned unchanged.
//
//	chrome-extension://efaidnb.../https://justice.gov.et/x.pdf
//	                            → https://justice.gov.et/x.pdf
func stripChromeViewer(raw string) string {
	if !strings.HasPrefix(strings.ToLower(raw), chromeViewerPrefix) {
		return raw
	}
	// The inner URL is the remainder from its own scheme onward. Searching for
	// the scheme rather than splitting on "/" keeps a path that itself contains
	// "http" from being cut in the wrong place.
	for _, scheme := range []string{"https://", "http://"} {
		if i := strings.Index(raw, scheme); i > 0 {
			return raw[i:]
		}
	}
	return raw
}

// normalizeURL turns an href found on a page into an absolute, request-ready
// URL, resolving it against the page it was found on.
//
// Unicode survives this. Ethiopian legal PDFs are routinely named in Amharic
// (አዋጅ-ቁጥር-1234-2013.pdf), and both the raw and percent-encoded forms arrive in
// the wild. url.Parse keeps the decoded path in Path and the original encoding
// in RawPath, and String() reproduces whichever the server will accept — so a
// URL that was already encoded round-trips byte for byte, and a raw one gets
// encoded once. Nothing is "sanitised": stripping a character here would
// request a file that does not exist.
func normalizeURL(base *url.URL, href string) (request, display string, err error) {
	href = strings.TrimSpace(stripChromeViewer(href))
	if href == "" {
		return "", "", fmt.Errorf("empty href")
	}

	ref, err := url.Parse(href)
	if err != nil {
		return "", "", fmt.Errorf("parse href %q: %w", href, err)
	}

	resolved := ref
	if base != nil {
		resolved = base.ResolveReference(ref)
	}
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported scheme %q in %q", resolved.Scheme, href)
	}

	return resolved.String(), displayURL(resolved), nil
}

// displayURL renders a URL with its path decoded, for logs and for humans
// reading an Amharic filename.
//
// Built by hand rather than via URL.String(), which always re-encodes: it
// escapes Path when RawPath is empty, so there is no way to ask it for the
// readable form. The result is for display only and must never be requested —
// an unencoded path is not a valid request target.
func displayURL(u *url.URL) string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	b.WriteString(u.Path) // decoded
	if u.RawQuery != "" {
		b.WriteString("?")
		b.WriteString(u.RawQuery)
	}
	return b.String()
}

// looksLikePDFPath reports whether a URL's path names a PDF file. A weak
// signal, used for ordering candidates and never as proof: validation reads the
// response, not the name (see Validate).
func looksLikePDFPath(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	// u.Path is decoded, so this works for both %E1%8A%A0….pdf and አ….pdf.
	return strings.HasSuffix(strings.ToLower(u.Path), ".pdf")
}

// PDFCandidates finds every plausible PDF URL on a law page, best first.
//
// Pure: it parses HTML and returns URLs, making no requests. That is what lets
// the whole priority table be tested against fixtures. Deciding which candidate
// actually serves a PDF requires fetching it, which is Resolver.Resolve.
//
// The ordering is the entire point of this function. The Ministry's pages carry
// a JetElements button (`?jet_download=<hash>`) that looks like the download
// link and frequently answers HTTP 500, while the real file is linked from a
// separate `a.jepe-download-button` in the same page. Taking the first anchor
// that looks like a download gets it wrong most of the time.
func PDFCandidates(lawPageURL string, page []byte) ([]Candidate, error) {
	base, err := url.Parse(lawPageURL)
	if err != nil {
		return nil, fmt.Errorf("parse law page url %q: %w", lawPageURL, err)
	}

	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("parse law page html: %w", err)
	}

	var (
		out []Candidate
		// at maps a normalised URL to its index in out.
		at = map[string]int{}
	)
	add := func(href string, source CandidateSource, label string) {
		request, display, err := normalizeURL(base, href)
		if err != nil {
			return
		}

		// The same file is routinely linked more than once on a page — the real
		// Ministry page carries its PDF as a plain anchor *and* as the
		// jepe-download-button. Keep the best-sourced sighting rather than the
		// first: which element a URL came from is what decides priority, and
		// first-seen would file the download button's own link under a weaker
		// source purely because of where it sits in the markup.
		if i, seen := at[request]; seen {
			if source.priority() < out[i].Source.priority() {
				out[i].Source = source
				if label != "" {
					out[i].Label = label
				}
			}
			return
		}

		at[request] = len(out)
		out = append(out, Candidate{
			URL:     request,
			Display: display,
			Source:  source,
			Label:   label,
		})
	}

	for _, a := range elementsByTag(doc, "a") {
		href, ok := attr(a, "href")
		if !ok {
			continue
		}
		label := textOf(a)

		switch {
		case hasClass(a, "jepe-download-button"):
			add(href, SourceDownloadButton, label)
		case looksLikePDFPath(stripChromeViewer(href)):
			add(href, SourceDirectPDF, label)
		case hasClass(a, "jet-download") || IsJetDownload(href):
			// Recorded so a page with no other link still has something to
			// probe, and so the attempt is visible in the log.
			add(href, SourceJetDownload, label)
		}
	}

	// Some pages inline the document in a viewer instead of linking it.
	for _, tag := range []string{"iframe", "embed", "object"} {
		for _, n := range elementsByTag(doc, tag) {
			for _, name := range []string{"src", "data"} {
				if src, ok := attr(n, name); ok && looksLikePDFPath(stripChromeViewer(src)) {
					add(src, SourceEmbedded, "")
				}
			}
		}
	}

	// Stable: equal priorities keep document order, so the first download
	// button on a page stays the first candidate.
	stableSortByPriority(out)
	return out, nil
}

// stableSortByPriority orders candidates by source priority, preserving
// document order within each group. Insertion sort — these slices hold a
// handful of elements, and this keeps the ordering rule readable in one place.
func stableSortByPriority(c []Candidate) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].Source.priority() < c[j-1].Source.priority(); j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}
