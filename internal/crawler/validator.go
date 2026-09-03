package crawler

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

// pdfMagic is the first five bytes of every PDF file. The version digits follow
// it ("%PDF-1.7"), and are not checked: this establishes the file type, not
// which spec revision produced it.
var pdfMagic = []byte("%PDF-")

// peekSize is how much of a response is read to identify it. A PDF header lands
// in the first bytes; this is generously more, so an error page can also be
// recognised and quoted back in the error.
const peekSize = 1024

// MinPDFSize rejects a "PDF" too small to be a legal document. Real
// proclamations run to tens of kilobytes at least; anything under this is a
// stub, a truncated transfer, or an error page that happens to start correctly.
const MinPDFSize = 1024

// ErrNotPDF marks a response that is not a PDF. Not a transport failure: the
// request worked, and the thing at the other end is something else — usually
// an HTML error page served with HTTP 200, which is the failure mode that makes
// extension-based checks useless here.
var ErrNotPDF = errors.New("not a PDF")

// ValidateContentType reports whether a Content-Type header names a PDF.
//
// Advisory only. Some Ministry uploads are served as application/octet-stream,
// so a failure here is not decisive — the magic bytes are. It is used to reject
// the clear negatives, above all text/html.
func ValidateContentType(header string) (ok bool, definite bool) {
	if header == "" {
		return false, false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false, false
	}
	switch strings.ToLower(mediaType) {
	case "application/pdf", "application/x-pdf":
		return true, true
	case "text/html", "application/xhtml+xml", "application/json", "text/plain":
		// A definite no. This is the WordPress or Cloudflare error page.
		return false, true
	default:
		// octet-stream and friends: undecided, let the bytes settle it.
		return false, false
	}
}

// ValidatePDFHeader reports whether a response body begins with the PDF magic
// bytes.
//
// Leading whitespace is tolerated because some servers prepend a newline; more
// than a few bytes of anything else is not a PDF with a quirk, it is a
// different file.
func ValidatePDFHeader(peek []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(peek, " \t\r\n"), pdfMagic)
}

// ValidateResponse decides whether a fetched response is a usable PDF, given
// its headers and the first bytes of its body.
//
// The rule is content, never extension. A URL ending in .pdf that serves an
// HTML error page is the single most common way a crawl silently poisons a
// corpus: the file lands on disk, the extractor reads no articles, and the
// document is quarantined for the wrong reason — if anyone notices at all.
func ValidateResponse(resp *http.Response, peek []byte) error {
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrNotPDF, resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	typeSaysPDF, definite := ValidateContentType(contentType)
	if definite && !typeSaysPDF {
		return fmt.Errorf("%w: Content-Type %q%s", ErrNotPDF, contentType, describe(peek))
	}

	if !ValidatePDFHeader(peek) {
		return fmt.Errorf("%w: body does not start with %%PDF-%s", ErrNotPDF, describe(peek))
	}

	// A declared length below the floor is worth catching before the transfer,
	// though the real check is on bytes written — see Downloader.Download.
	if resp.ContentLength > 0 && resp.ContentLength < MinPDFSize {
		return fmt.Errorf("%w: %d bytes is too small for a legal document",
			ErrNotPDF, resp.ContentLength)
	}

	return nil
}

// describe quotes the start of an unexpected body, so a failure names what
// arrived instead of only what did not.
func describe(peek []byte) string {
	snippet := strings.TrimSpace(string(peek))
	if snippet == "" {
		return " (empty body)"
	}
	snippet = strings.Join(strings.Fields(snippet), " ")
	if len(snippet) > 120 {
		snippet = snippet[:120] + "…"
	}
	return fmt.Sprintf(" (body starts %q)", snippet)
}
