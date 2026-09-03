package rag

import "github.com/Seera-et/seera-backend-services/internal/search"

// ConfidenceLevel summarises how much evidence backs an answer that is about
// to be generated. It is read from signals retrieval and reranking already
// produced — no new retrieval logic and no extra model call.
type ConfidenceLevel string

const (
	ConfidenceHigh   ConfidenceLevel = "high"
	ConfidenceMedium ConfidenceLevel = "medium"
	ConfidenceLow    ConfidenceLevel = "low"
)

// Confidence is attached to a Result so the client — and the eval harness —
// can see how much the answer is actually resting on.
type Confidence struct {
	Level ConfidenceLevel `json:"level"`

	// ExactMatch is true when at least one source came from a deterministic
	// article-number lookup rather than a similarity judgement.
	ExactMatch bool `json:"exact_match"`

	SourceCount int `json:"source_count"`
}

// confidenceFrom is only ever called once shouldAbstain has already passed, so
// it only ever describes an answer that is actually about to be generated.
func confidenceFrom(top []search.Result, sourceCount int, minRerankScore float64) Confidence {
	if len(top) == 0 {
		return Confidence{Level: ConfidenceLow, SourceCount: sourceCount}
	}

	var exact, agreed bool
	for _, r := range top {
		switch r.Retriever {
		case search.RetrieverArticle:
			exact = true
		case search.RetrieverHybrid:
			agreed = true
		}
	}

	// Only meaningful with a calibrated reranker (Cohere); with the passthrough
	// reranker the score is an uncalibrated RRF value, so minRerankScore is
	// zero and this comparison never fires — the same guard Config documents
	// for abstention.
	//
	// Read through bestRerankedScore rather than off top[0]: a carried source
	// leads the list with a zero Score that nothing judged, which would report
	// every follow-up as weakly supported no matter how strong its retrieval.
	best, scored := bestRerankedScore(top)
	calibratedStrong := minRerankScore > 0 && scored && best >= minRerankScore*1.5

	level := ConfidenceMedium
	switch {
	case exact || calibratedStrong || (agreed && len(top) > 1):
		level = ConfidenceHigh
	case len(top) == 1:
		level = ConfidenceLow
	}

	return Confidence{Level: level, ExactMatch: exact, SourceCount: sourceCount}
}
