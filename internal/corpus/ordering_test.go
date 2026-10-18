package corpus

import (
	"testing"
	"time"

	"github.com/Seera-et/seera-backend-services/internal/crawler"
)

// The publish rule is "first one wins", so the order documents arrive in
// decides which edition of a law ends up live. Left to the filesystem, that
// would be the alphabetical order of Amharic slugs — which reads as a bug when
// someone looks at the report and asks why the 1960 code is the published one.
//
// Oldest first means a later edition arrives second and is correctly held as a
// draft for a human to promote.
func TestOrderedPutsOldestPublicationFirst(t *testing.T) {
	day := func(s string) *time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}

	in := []crawler.Record{
		{DocID: "newer", Page: crawler.LawPage{PublicationDate: day("2021-03-25")}},
		{DocID: "undated"},
		{DocID: "older", Page: crawler.LawPage{PublicationDate: day("1960-05-05")}},
	}

	var got []string
	for _, r := range ordered(in) {
		got = append(got, r.DocID)
	}

	want := []string{"older", "newer", "undated"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordered() = %v, want %v", got, want)
		}
	}
}

func TestOrderedIsStableForUndatedDocuments(t *testing.T) {
	in := []crawler.Record{{DocID: "zeta"}, {DocID: "alpha"}, {DocID: "mid"}}

	var got []string
	for _, r := range ordered(in) {
		got = append(got, r.DocID)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordered() = %v, want %v", got, want)
		}
	}
}

// Sorting must not rearrange the caller's slice: the driver reports from the
// store's own listing elsewhere.
func TestOrderedDoesNotMutateItsInput(t *testing.T) {
	in := []crawler.Record{{DocID: "zeta"}, {DocID: "alpha"}}
	_ = ordered(in)
	if in[0].DocID != "zeta" {
		t.Error("ordered() reordered the caller's slice")
	}
}
