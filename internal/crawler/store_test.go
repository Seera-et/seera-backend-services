package crawler

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestRecordRoundTrips(t *testing.T) {
	s := testStore(t)

	want := Record{
		DocID: "federal-courts-procla",
		State: StateReady,
		Page: LawPage{
			Ref:            LawPageRef{URL: lawPage, Slug: "federal-courts-procla", Language: "en"},
			TitleEn:        "Federal Courts Proclamation",
			TitleAm:        "የፌዴራል ፍርድ ቤቶች አዋጅ",
			DocumentNumber: "1234/2021",
			Status:         "In Force",
		},
		OriginalFileURL: "https://justice.gov.et/wp-content/uploads/2025/02/x.pdf",
		Checksum:        strings.Repeat("a", 64),
		Bytes:           123456,
	}

	if err := s.SaveRecord(want); err != nil {
		t.Fatalf("SaveRecord: %v", err)
	}

	got, found, err := s.LoadRecord(want.DocID)
	if err != nil || !found {
		t.Fatalf("LoadRecord: found=%v err=%v", found, err)
	}
	if got.Checksum != want.Checksum || got.State != want.State {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
	if got.Page.TitleAm != want.Page.TitleAm {
		t.Errorf("Amharic title did not survive the round trip: %q", got.Page.TitleAm)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}
}

func TestLoadMissingRecord(t *testing.T) {
	_, found, err := testStore(t).LoadRecord("never-crawled")
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}
	if found {
		t.Error("reported a record for a document that was never crawled")
	}
}

// The Ministry links the same consolidated PDF from more than one law page.
// Ingesting it twice would put every article under two document identities.
func TestChecksumDeduplication(t *testing.T) {
	s := testStore(t)
	sum := strings.Repeat("b", 64)

	if owner, duplicate := s.ClaimChecksum(sum, "first-law"); duplicate || owner != "first-law" {
		t.Fatalf("first claim reported owner=%q duplicate=%v", owner, duplicate)
	}

	owner, duplicate := s.ClaimChecksum(sum, "second-law")
	if !duplicate {
		t.Error("the same PDF under a second law page was not flagged as a duplicate")
	}
	if owner != "first-law" {
		t.Errorf("duplicate points at %q, want the document that already held it", owner)
	}

	// Re-claiming for the same document is a re-crawl, not a duplicate.
	if _, duplicate := s.ClaimChecksum(sum, "first-law"); duplicate {
		t.Error("a document re-crawled was reported as a duplicate of itself")
	}
}

// A crawl of this size gets interrupted. Dedupe has to survive the restart, or
// the second run re-ingests everything the first one already had.
func TestChecksumIndexSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	sum := strings.Repeat("c", 64)

	first, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	first.ClaimChecksum(sum, "already-have-this")
	if err := first.SaveChecksums(); err != nil {
		t.Fatalf("SaveChecksums: %v", err)
	}

	second, err := NewStore(root)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	owner, duplicate := second.ClaimChecksum(sum, "a-different-law")
	if !duplicate || owner != "already-have-this" {
		t.Errorf("after restart: owner=%q duplicate=%v, want the earlier document", owner, duplicate)
	}
}

func TestStorePathsAreDeterministic(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"raw", s.PDFPath("abc"), filepath.Join(root, "raw", "abc", "original.pdf")},
		{"extracted", s.TextPath("abc"), filepath.Join(root, "extracted", "abc", "text.txt")},
		{"structured", s.StructuredPath("abc"), filepath.Join(root, "structured", "abc", "document.json")},
		{"metadata", s.recordPath("abc"), filepath.Join(root, "metadata", "abc.json")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s path = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// Slugs come from the Ministry's URLs, decoded. They are the law's Amharic
// title, they run long, and nothing guarantees they are usable as a directory
// name — so they are sanitised before they touch the filesystem.
func TestSafeDocID(t *testing.T) {
	t.Run("stays readable and deterministic", func(t *testing.T) {
		slug := "የኢትዮጵያ-ስታርታፕ-አዋጅ-ቁጥር-1396-2017-ethiopian-startup-procla"
		got := SafeDocID(slug)

		if got != SafeDocID(slug) {
			t.Error("not deterministic")
		}
		if !strings.HasPrefix(got, "የኢትዮጵያ-ስታርታፕ") {
			t.Errorf("lost the readable prefix: %q", got)
		}
	})

	t.Run("a decoded separator cannot escape the store", func(t *testing.T) {
		for _, slug := range []string{
			"../../etc/passwd",
			"..\\..\\windows\\system32",
			"a/b/c",
			"..",
			".",
		} {
			got := SafeDocID(slug)
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("SafeDocID(%q) = %q, still contains a path separator", slug, got)
			}
			if got == ".." || got == "." || strings.HasPrefix(got, "..") {
				t.Errorf("SafeDocID(%q) = %q, still traverses", slug, got)
			}
			if filepath.Base(got) != got {
				t.Errorf("SafeDocID(%q) = %q, is not a single path segment", slug, got)
			}
		}
	})

	t.Run("windows-reserved characters are removed", func(t *testing.T) {
		got := SafeDocID(`law:with*bad?chars"<>|`)
		if strings.ContainsAny(got, `:*?"<>|`) {
			t.Errorf("SafeDocID kept a reserved character: %q", got)
		}
	})

	t.Run("long slugs are capped but stay unique", func(t *testing.T) {
		// Ethiopian proclamation titles routinely share a long opening phrase.
		a := strings.Repeat("የኢትዮጵያ-", 30) + "first"
		b := strings.Repeat("የኢትዮጵያ-", 30) + "second"

		idA, idB := SafeDocID(a), SafeDocID(b)
		if idA == idB {
			t.Error("two laws sharing a long prefix collided onto one directory")
		}
		for _, id := range []string{idA, idB} {
			if len([]rune(id)) > maxDocIDRunes+docIDHashLen+1 {
				t.Errorf("id is %d runes, longer than the cap: %q", len([]rune(id)), id)
			}
		}
	})

	t.Run("a slug that sanitises to nothing still gets an id", func(t *testing.T) {
		if got := SafeDocID("///"); got == "" {
			t.Error("empty id for a slug that sanitised away")
		}
	})
}

func TestRecordFail(t *testing.T) {
	var r Record
	r.Fail(ReasonInvalidPDF, ErrNotPDF)

	if r.State != StateFailed {
		t.Errorf("state = %q, want %q", r.State, StateFailed)
	}
	if r.Reason != ReasonInvalidPDF {
		t.Errorf("reason = %q, want %q", r.Reason, ReasonInvalidPDF)
	}
	if r.Error == "" {
		t.Error("a failure must record what went wrong, not only that it did")
	}
}

// The category exists so a thousand-document run can be counted by cause. The
// Ministry's own broken button is not the same problem as a page that never had
// a document, and the two need different responses.
func TestClassifyDownloadFailure(t *testing.T) {
	jetOnly := []Candidate{{Source: SourceJetDownload}}
	mixed := []Candidate{{Source: SourceDownloadButton}, {Source: SourceJetDownload}}

	tests := []struct {
		name       string
		candidates []Candidate
		err        error
		want       FailureReason
	}{
		{"only the broken jet button", jetOnly, ErrNoPDF, ReasonJetDownloadFailed},
		{"real links that served something else", mixed, ErrNotPDF, ReasonInvalidPDF},
		{"real links that never resolved", mixed, ErrNoPDF, ReasonNoPDFLink},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDownloadFailure(tt.candidates, tt.err); got != tt.want {
				t.Errorf("classifyDownloadFailure = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStateTerminal(t *testing.T) {
	terminal := []State{StateReady, StateFailed}
	ongoing := []State{StateDiscovered, StateFetchedPage, StatePDFFound, StatePDFValid, StateOCRRequired}

	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range ongoing {
		if s.Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}
