package rag

import (
	"slices"
	"strings"
	"testing"

	"github.com/Seera-et/seera-backend-services/internal/legal"
	"github.com/Seera-et/seera-backend-services/internal/search"
)

func testSources(n int) []SourceRef {
	refs := make([]SourceRef, n)
	for i := range refs {
		refs[i] = SourceRef{
			Marker: "S" + string(rune('1'+i)),
			Result: search.Result{ChunkID: legal.ChunkID("chunk-" + string(rune('1'+i)))},
		}
	}
	return refs
}

// Feeding the answer through the filter one rune at a time is the realistic
// case: a marker routinely arrives split across two network chunks.
func writeInChunks(f *MarkerFilter, text string, size int) string {
	var out strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i += size {
		end := min(i+size, len(runes))
		out.WriteString(f.Write(string(runes[i:end])))
	}
	out.WriteString(f.Close())
	return out.String()
}

func TestMarkerFilterPassesValidMarkers(t *testing.T) {
	f := NewMarkerFilter(testSources(3))

	got := writeInChunks(f, "A company may be formed [S1] and must register [S2].", 3)

	if !strings.Contains(got, "[S1]") || !strings.Contains(got, "[S2]") {
		t.Errorf("valid markers were altered: %q", got)
	}
	if want := []string{"S1", "S2"}; strings.Join(f.Used(), ",") != strings.Join(want, ",") {
		t.Errorf("Used() = %v, want %v", f.Used(), want)
	}
	if f.Invalid() != 0 {
		t.Errorf("Invalid() = %d, want 0", f.Invalid())
	}
}

// The core grounding guarantee: a marker naming a source that was never in the
// context must not reach the client. Post-hoc validation cannot deliver this,
// because by then the text is already on screen.
func TestMarkerFilterDropsInventedMarkers(t *testing.T) {
	f := NewMarkerFilter(testSources(2))

	got := writeInChunks(f, "The period is thirty days [S7].", 1)

	if strings.Contains(got, "S7") {
		t.Errorf("invented marker reached the client: %q", got)
	}
	if !strings.Contains(got, "thirty days") {
		t.Errorf("filter ate the surrounding text: %q", got)
	}
	if f.Invalid() != 1 {
		t.Errorf("Invalid() = %d, want 1", f.Invalid())
	}
	if len(f.Used()) != 0 {
		t.Errorf("Used() = %v, want empty", f.Used())
	}
}

func TestMarkerFilterKeepsValidHalfOfAGroup(t *testing.T) {
	f := NewMarkerFilter(testSources(2))

	got := writeInChunks(f, "Both apply [S1, S9].", 2)

	if !strings.Contains(got, "[S1]") {
		t.Errorf("valid marker in a mixed group was dropped: %q", got)
	}
	if strings.Contains(got, "S9") {
		t.Errorf("invalid marker in a mixed group survived: %q", got)
	}
}

func TestMarkerFilterHandlesMultipleValidMarkersInOneGroup(t *testing.T) {
	f := NewMarkerFilter(testSources(3))

	got := writeInChunks(f, "This is established [S1, S3].", 4)

	if !strings.Contains(got, "[S1, S3]") {
		t.Errorf("group of valid markers was mangled: %q", got)
	}
	if len(f.Used()) != 2 {
		t.Errorf("Used() = %v, want two markers", f.Used())
	}
}

// Brackets that are not citations are part of the law's own text — sub-article
// references like "[1]" or bracketed clarifications — and must survive.
func TestMarkerFilterLeavesNonCitationBracketsAlone(t *testing.T) {
	f := NewMarkerFilter(testSources(2))

	for _, text := range []string{
		"Sub-article [1] applies.",
		"The word [sic] appears in the original.",
		"An unclosed bracket [ stays put.",
	} {
		t.Run(text, func(t *testing.T) {
			filter := NewMarkerFilter(testSources(2))
			got := writeInChunks(filter, text, 3)
			if got != text {
				t.Errorf("text was altered:\n got %q\nwant %q", got, text)
			}
		})
	}
	_ = f
}

func TestMarkerFilterDeduplicatesUsedMarkers(t *testing.T) {
	f := NewMarkerFilter(testSources(2))

	writeInChunks(f, "First [S1]. Again [S1]. And [S2].", 5)

	if want := "S1,S2"; strings.Join(f.Used(), ",") != want {
		t.Errorf("Used() = %v, want first-seen order without duplicates", f.Used())
	}
}

// A bracket that never closes must not swallow the rest of the answer.
func TestMarkerFilterReleasesAnUnterminatedBracket(t *testing.T) {
	f := NewMarkerFilter(testSources(1))
	long := "[" + strings.Repeat("x", maxMarkerLength+20)

	got := writeInChunks(f, long, 7)

	if len(got) != len(long) {
		t.Errorf("filter withheld text: got %d chars, want %d", len(got), len(long))
	}
}

func TestLooksSubstantive(t *testing.T) {
	if looksSubstantive("I could not find that.") {
		t.Error("a short refusal should not be treated as making legal claims")
	}
	if !looksSubstantive(strings.Repeat("The law provides that a company shall register. ", 10)) {
		t.Error("a long answer should be treated as making legal claims")
	}
}

func TestSystemPromptCarriesNoLegalContent(t *testing.T) {
	// The architecture rule: legal information lives in the database or it does
	// not exist. A prompt that names an article has hardcoded the law, and it
	// cannot then be amended without a deploy.
	for _, forbidden := range []string{"Article 4", "Proclamation No", "shall be thirty days"} {
		if strings.Contains(SystemPrompt, forbidden) {
			t.Errorf("system prompt contains legal content: %q", forbidden)
		}
	}
	for _, required := range []string{"only the numbered sources", "[S1]", "not legal advice"} {
		if !strings.Contains(SystemPrompt, required) {
			t.Errorf("system prompt is missing its contract: %q", required)
		}
	}
}

// The failure this guards against was observed in production: the model wrote
// "[S1, Art. 265(1)]", the filter did not recognise it as a citation, and a
// fully cited answer was reported to the reader as partly uncited.
func TestMarkerFilterAcceptsDecoratedMarkers(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantText string
		wantUsed []string
	}{
		{
			name:     "plain marker",
			in:       "Registration is required [S1].",
			wantText: "Registration is required [S1].",
			wantUsed: []string{"S1"},
		},
		{
			name:     "marker with an article reference the model added",
			in:       "Registration is required [S1, Art. 265(1)].",
			wantText: "Registration is required [S1].",
			wantUsed: []string{"S1"},
		},
		{
			name:     "group with a trailing reference",
			in:       "Both apply [S1, S2, Art. 499].",
			wantText: "Both apply [S1, S2].",
			wantUsed: []string{"S1", "S2"},
		},
		{
			name:     "lower case",
			in:       "As stated [s2].",
			wantText: "As stated [S2].",
			wantUsed: []string{"S2"},
		},
		{
			name:     "decoration on an invented source is dropped with it",
			in:       "A claim [S9, Art. 12].",
			wantText: "A claim .",
			wantUsed: nil,
		},
		{
			name:     "bracketed prose is left alone",
			in:       "See [the schedule] for details.",
			wantText: "See [the schedule] for details.",
			wantUsed: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewMarkerFilter([]SourceRef{{Marker: "S1"}, {Marker: "S2"}})

			var out strings.Builder
			out.WriteString(f.Write(tt.in))
			out.WriteString(f.Close())

			if got := out.String(); got != tt.wantText {
				t.Errorf("text = %q, want %q", got, tt.wantText)
			}
			if got := f.Used(); !slices.Equal(got, tt.wantUsed) {
				t.Errorf("used = %v, want %v", got, tt.wantUsed)
			}
		})
	}
}
