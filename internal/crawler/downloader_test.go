package crawler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePDF is a byte sequence that passes validation: correct magic bytes and
// comfortably over MinPDFSize.
func fakePDF() []byte {
	return append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("legal text "), 400)...)
}

func testDownloader(t *testing.T) *Downloader {
	t.Helper()
	return NewDownloader(NewClient(testClientConfig(), nil), nil)
}

// The scenario the whole resolver exists for, end to end: the JetElements
// endpoint answers 500 and the real link, on the same page, serves the file.
// The document must survive.
func TestJetDownloadFailureFallsThroughToTheRealPDF(t *testing.T) {
	var jetHits, pdfHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/en/", func(w http.ResponseWriter, r *http.Request) {
		jetHits++
		http.Error(w, "jet download broke", http.StatusInternalServerError)
	})
	mux.HandleFunc("/wp-content/uploads/doc.pdf", func(w http.ResponseWriter, r *http.Request) {
		pdfHits++
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(fakePDF())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Jet first in the list, so only the fall-through can succeed.
	candidates := []Candidate{
		{URL: srv.URL + "/en/?jet_download=abc", Display: "jet", Source: SourceJetDownload},
		{URL: srv.URL + "/wp-content/uploads/doc.pdf", Display: "doc.pdf", Source: SourceDirectPDF},
	}

	dest := filepath.Join(t.TempDir(), "original.pdf")
	got, err := testDownloader(t).Fetch(context.Background(), candidates, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got.Candidate.Source != SourceDirectPDF {
		t.Errorf("downloaded from %q, want the real link", got.Candidate.Source)
	}
	if jetHits == 0 {
		t.Error("never tried the jet endpoint, so the fall-through was not exercised")
	}
	if pdfHits != 1 {
		t.Errorf("fetched the real PDF %d times, want 1", pdfHits)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("no file written: %v", err)
	}
}

// The failure mode that quietly poisons a corpus: HTTP 200, a .pdf URL, and an
// HTML error page in the body.
func TestHTMLServedAsPDFIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><h1>Page not found</h1></body></html>`))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "original.pdf")
	_, err := testDownloader(t).Fetch(context.Background(),
		[]Candidate{{URL: srv.URL + "/doc.pdf", Display: "doc.pdf", Source: SourceDirectPDF}}, dest)

	if !errors.Is(err, ErrNoPDF) {
		t.Fatalf("error = %v, want ErrNoPDF", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("an HTML error page was written into the corpus directory")
	}
}

// Content-Type can be octet-stream on real uploads, so the magic bytes have the
// final say — in both directions.
func TestValidationPrefersMagicBytesOverContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantOK      bool
	}{
		{"pdf served as octet-stream", "application/octet-stream", fakePDF(), true},
		{"pdf with no content type at all", "", fakePDF(), true},
		{"html claiming to be a pdf", "application/pdf", []byte("<html>nope</html>"), false},
		{"json error body", "application/json", []byte(`{"error":"nope"}`), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()

			dest := filepath.Join(t.TempDir(), "original.pdf")
			_, err := testDownloader(t).Fetch(context.Background(),
				[]Candidate{{URL: srv.URL + "/doc.pdf", Source: SourceDirectPDF}}, dest)

			if tt.wantOK && err != nil {
				t.Errorf("rejected a valid PDF: %v", err)
			}
			if !tt.wantOK && err == nil {
				t.Error("accepted a response that is not a PDF")
			}
		})
	}
}

// A transfer cut short still starts with %PDF-, so the size floor is checked on
// what actually arrived.
func TestTruncatedPDFIsRejectedAndNotKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\nthat's all folks"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "original.pdf")
	_, err := testDownloader(t).Fetch(context.Background(),
		[]Candidate{{URL: srv.URL + "/doc.pdf", Source: SourceDirectPDF}}, dest)

	if !errors.Is(err, ErrNoPDF) {
		t.Fatalf("error = %v, want ErrNoPDF", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("a truncated PDF was left in the corpus directory")
	}
}

func TestChecksumIsStableAndCorrect(t *testing.T) {
	body := fakePDF()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := testDownloader(t)

	first, err := d.Fetch(context.Background(),
		[]Candidate{{URL: srv.URL + "/a.pdf", Source: SourceDirectPDF}}, filepath.Join(dir, "a.pdf"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	second, err := d.Fetch(context.Background(),
		[]Candidate{{URL: srv.URL + "/b.pdf", Source: SourceDirectPDF}}, filepath.Join(dir, "b.pdf"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if first.Checksum != second.Checksum {
		t.Errorf("the same bytes hashed differently:\n %s\n %s", first.Checksum, second.Checksum)
	}
	if len(first.Checksum) != 64 {
		t.Errorf("checksum %q is not a sha256 hex digest", first.Checksum)
	}
	if first.Bytes != int64(len(body)) {
		t.Errorf("recorded %d bytes, want %d", first.Bytes, len(body))
	}

	written, err := os.ReadFile(filepath.Join(dir, "a.pdf"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(written, body) {
		t.Error("the file on disk is not what the server sent")
	}
}

func TestFetchWithNoCandidates(t *testing.T) {
	_, err := testDownloader(t).Fetch(context.Background(), nil, filepath.Join(t.TempDir(), "x.pdf"))
	if !errors.Is(err, ErrNoPDF) {
		t.Errorf("error = %v, want ErrNoPDF", err)
	}
}

// Every candidate failing must say what each one did, or a thousand-document
// crawl produces a thousand unactionable failures.
func TestExhaustedCandidatesReportEveryAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>no</html>"))
	}))
	defer srv.Close()

	_, err := testDownloader(t).Fetch(context.Background(), []Candidate{
		{URL: srv.URL + "/one.pdf", Display: "one.pdf", Source: SourceDirectPDF},
		{URL: srv.URL + "/two.pdf", Display: "two.pdf", Source: SourceEmbedded},
	}, filepath.Join(t.TempDir(), "x.pdf"))

	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"one.pdf", "two.pdf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestValidateContentType(t *testing.T) {
	tests := []struct {
		header       string
		wantOK       bool
		wantDefinite bool
	}{
		{"application/pdf", true, true},
		{"application/pdf; charset=binary", true, true},
		{"text/html; charset=UTF-8", false, true},
		{"application/octet-stream", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		ok, definite := ValidateContentType(tt.header)
		if ok != tt.wantOK || definite != tt.wantDefinite {
			t.Errorf("ValidateContentType(%q) = (%v, %v), want (%v, %v)",
				tt.header, ok, definite, tt.wantOK, tt.wantDefinite)
		}
	}
}

func TestValidatePDFHeader(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"plain", "%PDF-1.7\n...", true},
		{"leading newline some servers add", "\n%PDF-1.4", true},
		{"html", "<!DOCTYPE html>", false},
		{"empty", "", false},
		{"pdf mentioned but not leading", "see the %PDF- spec", false},
	}
	for _, tt := range tests {
		if got := ValidatePDFHeader([]byte(tt.body)); got != tt.want {
			t.Errorf("%s: ValidatePDFHeader(%q) = %v, want %v", tt.name, tt.body, got, tt.want)
		}
	}
}

// An error must quote what arrived instead. "Not a PDF" alone does not tell an
// operator whether the site moved, blocked them, or served a login page.
func TestRejectionQuotesTheBodyItGot(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
	}
	err := ValidateResponse(resp, []byte("<html><body>Access denied</body></html>"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("error does not quote the body: %v", err)
	}
}

func TestProbeAcceptsPartialContent(t *testing.T) {
	body := fakePDF()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("probe did not ask for a byte range")
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", peekSize-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:peekSize])
	}))
	defer srv.Close()

	if err := testDownloader(t).Probe(context.Background(),
		Candidate{URL: srv.URL + "/doc.pdf", Source: SourceDirectPDF}); err != nil {
		t.Errorf("Probe: %v", err)
	}
}
