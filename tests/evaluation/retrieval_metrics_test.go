//go:build eval

package evaluation

import "testing"

// The per-language split exists because a blended average can hide an Amharic
// collapse behind English results — the exact risk docs/QA_FEATURE_PLAN.md §10
// names and §8 sets a separate 0.65 gate for. These pin the arithmetic, and run
// without a database or an API key.

func TestRetrievalTallyMetrics(t *testing.T) {
	// 4 cases: 3 found in the fused list, 2 survive to the reranked top 10.
	r := &retrievalTally{
		total:            4,
		fusedRecallHits:  3,
		rerankRecallHits: 2,
		precisionSum:     0.4 + 0.2 + 0.2 + 0.0, // per-case P@5
		reciprocalRankSum: 1.0 + // rank 1
			0.5 + // rank 2
			0.25, // rank 4
		hit1: 1,
		hit3: 2,
	}

	tests := []struct {
		name string
		got  float64
		want float64
	}{
		{"Recall@50", r.recallAt50(), 0.75},
		{"Recall@10", r.recallAt10(), 0.50},
		{"Precision@5", r.precisionAt5(), 0.20},
		{"MRR", r.mrr(), 0.4375},
		{"Hit@1", r.hitAt1(), 0.25},
		{"Hit@3", r.hitAt3(), 0.50},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

// An empty bucket is the normal state of the Amharic bucket until Amharic
// documents are ingested. It must read as zero, not divide by zero.
func TestEmptyTallyDoesNotPanic(t *testing.T) {
	r := &retrievalTally{}
	for _, got := range []float64{
		r.recallAt50(), r.recallAt10(), r.precisionAt5(),
		r.mrr(), r.hitAt1(), r.hitAt3(),
	} {
		if got != 0 {
			t.Errorf("empty tally reported %v, want 0", got)
		}
	}
	if m := r.metrics("am"); m.Bucket != "am" || m.Cases != 0 {
		t.Errorf("metrics(%q) = %+v, want an empty am bucket", "am", m)
	}
}

// Each bucket must total independently — a language bucket that accidentally
// shared a counter with the overall tally would report the overall number
// under a language label, which is precisely the confusion the split removes.
func TestBucketsAccumulateIndependently(t *testing.T) {
	overall := &retrievalTally{}
	en := &retrievalTally{}
	am := &retrievalTally{}

	for _, bucket := range []*retrievalTally{overall, en} {
		bucket.total++
		bucket.fusedRecallHits++
		bucket.precisionSum += 1.0
	}
	// An Amharic case that retrieval missed entirely.
	for _, bucket := range []*retrievalTally{overall, am} {
		bucket.total++
	}

	if overall.total != 2 || en.total != 1 || am.total != 1 {
		t.Fatalf("totals = overall %d, en %d, am %d; want 2, 1, 1",
			overall.total, en.total, am.total)
	}
	if en.precisionAt5() != 1.0 {
		t.Errorf("English P@5 = %v, want 1.0", en.precisionAt5())
	}
	if am.precisionAt5() != 0.0 {
		t.Errorf("Amharic P@5 = %v, want 0.0", am.precisionAt5())
	}
	// The blended number that would have hidden the Amharic miss.
	if overall.precisionAt5() != 0.5 {
		t.Errorf("overall P@5 = %v, want 0.5", overall.precisionAt5())
	}
}
