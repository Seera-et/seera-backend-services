package search

import "sort"

// RRFConstant is the k in Reciprocal Rank Fusion.
//
// 60 is the value from the original paper and the field's default. It is not
// tuned here on purpose: fusing by rank with a fixed constant avoids the
// alternative, which is normalising a cosine similarity against a ts_rank_cd
// score — two numbers on unrelated scales whose relative weighting would need
// re-tuning every time either arm changes.
const RRFConstant = 60.0

// FuseRRF merges ranked candidate lists by Reciprocal Rank Fusion.
//
// Each list contributes 1/(k + rank) to every chunk it returns, and the
// contributions sum. A chunk that several arms rank highly beats one that a
// single arm loves, which is exactly the behaviour wanted from hybrid search:
// agreement between lexical and semantic retrieval is strong evidence, and
// disagreement is not fatal.
//
// Input lists must already be ordered best-first. Empty lists are ignored.
func FuseRRF(lists ...[]Result) []Result {
	type fused struct {
		result     Result
		score      float64
		retrievers map[Retriever]bool
	}

	byChunk := make(map[string]*fused)
	order := make([]string, 0, 16) // preserves first-seen order for stable ties

	for _, list := range lists {
		for rank, r := range list {
			key := string(r.ChunkID)
			contribution := 1.0 / (RRFConstant + float64(rank+1))

			entry, seen := byChunk[key]
			if !seen {
				entry = &fused{result: r, retrievers: map[Retriever]bool{}}
				byChunk[key] = entry
				order = append(order, key)
			}
			entry.score += contribution
			entry.retrievers[r.Retriever] = true
		}
	}

	out := make([]Result, 0, len(order))
	for _, key := range order {
		entry := byChunk[key]
		result := entry.result
		result.Score = entry.score

		// A chunk found by more than one arm no longer belongs to any single
		// one. Naming the arm that happened to be scanned first would be a lie
		// in exactly the case worth knowing about.
		if len(entry.retrievers) > 1 {
			result.Retriever = RetrieverHybrid
		}
		out = append(out, result)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
