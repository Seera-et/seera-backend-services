package crawler

import (
	"strings"
	"testing"
)

const lawPage = "https://justice.gov.et/en/law/federal-courts-procla/"

// The markup that motivates the whole resolver: the JetElements button and the
// real download link sit on the same page, and the one that looks like the
// download is the one that answers HTTP 500.
const realWorldPage = `<!DOCTYPE html>
<html><body>
  <div class="elementor-widget-container">
    <div class="elementor-jet-download-button jet-elements">
      <a class="elementor-button elementor-size-md jet-download jet-download-icon-position-left"
         href="https://justice.gov.et/en/?jet_download=cb734b9819fe23d3d244fc28d4ca0dc8616cd3af">
        <span class="elementor-button-text">Download</span>
      </a>
    </div>
  </div>
  <div class="jepe-download-button-container jepe-desktop-only">
    <a href="https://justice.gov.et/wp-content/uploads/2026/06/Pg-+TC-1415-2018.pdf"
       download target="_blank" class="jepe-download-button">
      <span>Download PDF</span>
    </a>
  </div>
</body></html>`

func TestPDFCandidatesPriority(t *testing.T) {
	tests := []struct {
		name       string
		html       string
		wantFirst  string
		wantSource CandidateSource
	}{
		{
			name:       "the real download button beats the jet endpoint on the same page",
			html:       realWorldPage,
			wantFirst:  "https://justice.gov.et/wp-content/uploads/2026/06/Pg-+TC-1415-2018.pdf",
			wantSource: SourceDownloadButton,
		},
		{
			name:       "a bare direct pdf link",
			html:       `<a href="https://justice.gov.et/wp-content/uploads/2026/06/doc.pdf">Download</a>`,
			wantFirst:  "https://justice.gov.et/wp-content/uploads/2026/06/doc.pdf",
			wantSource: SourceDirectPDF,
		},
		{
			name:       "a relative pdf url resolves against the law page",
			html:       `<a href="/wp-content/uploads/2026/06/document.pdf">PDF</a>`,
			wantFirst:  "https://justice.gov.et/wp-content/uploads/2026/06/document.pdf",
			wantSource: SourceDirectPDF,
		},
		{
			name: "a chrome pdf viewer url is unwrapped, never stored",
			html: `<a href="chrome-extension://efaidnbmnnnibpcajpcglclefindmkaj/` +
				`https://justice.gov.et/wp-content/uploads/2026/06/document.pdf">PDF</a>`,
			wantFirst:  "https://justice.gov.et/wp-content/uploads/2026/06/document.pdf",
			wantSource: SourceDirectPDF,
		},
		{
			name: "an already percent-encoded amharic filename round-trips unchanged",
			html: `<a class="jepe-download-button" href="https://justice.gov.et/wp-content/uploads/2025/02/` +
				`%E1%8A%A0%E1%8B%8B%E1%8C%85-1234-2013.pdf">Download PDF</a>`,
			wantFirst: "https://justice.gov.et/wp-content/uploads/2025/02/" +
				"%E1%8A%A0%E1%8B%8B%E1%8C%85-1234-2013.pdf",
			wantSource: SourceDownloadButton,
		},
		{
			name:       "an embedded viewer is used when there is no link",
			html:       `<iframe src="/wp-content/uploads/2026/06/inline.pdf"></iframe>`,
			wantFirst:  "https://justice.gov.et/wp-content/uploads/2026/06/inline.pdf",
			wantSource: SourceEmbedded,
		},
		{
			name:       "the jet endpoint is still offered when it is all there is",
			html:       `<a class="jet-download" href="https://justice.gov.et/en/?jet_download=abc123">Download</a>`,
			wantFirst:  "https://justice.gov.et/en/?jet_download=abc123",
			wantSource: SourceJetDownload,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PDFCandidates(lawPage, []byte(tt.html))
			if err != nil {
				t.Fatalf("PDFCandidates: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("no candidates found")
			}
			if got[0].URL != tt.wantFirst {
				t.Errorf("first candidate URL\n got %q\nwant %q", got[0].URL, tt.wantFirst)
			}
			if got[0].Source != tt.wantSource {
				t.Errorf("first candidate source = %q, want %q", got[0].Source, tt.wantSource)
			}
		})
	}
}

// The Chrome viewer URL is not a location anything can fetch. Storing one means
// a metadata record nobody can re-download from.
func TestChromeExtensionURLIsNeverStored(t *testing.T) {
	got, err := PDFCandidates(lawPage, []byte(realWorldPage+
		`<a href="chrome-extension://efaidnb/https://justice.gov.et/x.pdf">x</a>`))
	if err != nil {
		t.Fatalf("PDFCandidates: %v", err)
	}
	for _, c := range got {
		if strings.Contains(c.URL, "chrome-extension") || strings.Contains(c.Display, "chrome-extension") {
			t.Errorf("candidate kept a chrome-extension URL: %q", c.URL)
		}
	}
}

// A raw (unencoded) Amharic filename must be encoded for the request but stay
// readable in the log — losing either one costs a document or costs the ability
// to debug why.
func TestAmharicFilenameSurvives(t *testing.T) {
	page := `<a class="jepe-download-button" ` +
		`href="https://justice.gov.et/wp-content/uploads/2025/02/አዋጅ-ቁጥር-1234-2013.pdf">Download PDF</a>`

	got, err := PDFCandidates(lawPage, []byte(page))
	if err != nil {
		t.Fatalf("PDFCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}

	// Requested percent-encoded, because that is what the server accepts.
	if !strings.Contains(got[0].URL, "%E1%8A%A0") {
		t.Errorf("request URL is not percent-encoded: %q", got[0].URL)
	}
	// Shown decoded, because that is what a person can read.
	if !strings.Contains(got[0].Display, "አዋጅ") {
		t.Errorf("display URL lost the Amharic filename: %q", got[0].Display)
	}
}

func TestPageWithNoDownloadableLink(t *testing.T) {
	got, err := PDFCandidates(lawPage, []byte(`<a href="/en/laws/">Back to laws</a>`))
	if err != nil {
		t.Fatalf("PDFCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d candidates for a page with no PDF, want 0", len(got))
	}
}

func TestDuplicateLinksAreCollapsed(t *testing.T) {
	page := `<a class="jepe-download-button" href="/wp-content/uploads/a.pdf">PDF</a>
	         <a href="https://justice.gov.et/wp-content/uploads/a.pdf">Same file</a>`

	got, err := PDFCandidates(lawPage, []byte(page))
	if err != nil {
		t.Fatalf("PDFCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d candidates for one file linked twice, want 1", len(got))
	}
	if got[0].Source != SourceDownloadButton {
		t.Errorf("kept the %q link, want the download button", got[0].Source)
	}
}

func TestIsJetDownload(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://justice.gov.et/en/?jet_download=cb734b98", true},
		{"https://justice.gov.et/wp-content/uploads/2026/06/doc.pdf", false},
		{"https://justice.gov.et/en/law/federal-courts-procla/", false},
	}
	for _, tt := range tests {
		if got := IsJetDownload(tt.url); got != tt.want {
			t.Errorf("IsJetDownload(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

// hasClass must match whole tokens: the Ministry's jet button also carries
// "jet-download-icon-position-left", and a substring match would confuse the
// two classes on the element that matters most.
func TestClassMatchingIsTokenWise(t *testing.T) {
	page := `<a class="elementor-button jet-download jet-download-icon-position-left" ` +
		`href="https://justice.gov.et/en/?jet_download=x">D</a>`

	got, err := PDFCandidates(lawPage, []byte(page))
	if err != nil {
		t.Fatalf("PDFCandidates: %v", err)
	}
	if len(got) != 1 || got[0].Source != SourceJetDownload {
		t.Fatalf("got %+v, want one jet-download candidate", got)
	}
}
