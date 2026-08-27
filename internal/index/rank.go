package index

import (
	"math"
	"sort"
)

// Cosine returns the cosine similarity of two equal-length vectors. It
// returns 0 for a length mismatch (comparing embeddings from two different
// models) rather than panicking, since that is a configuration error the
// caller should detect up front, not a crash the search path should risk.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// rrfK is the standard damping constant from the original Reciprocal Rank
// Fusion paper (Cormack et al., 2009). Larger values flatten the influence
// of rank position; 60 is the widely used default and not worth exposing as
// a knob for a two-way fusion.
const rrfK = 60.0

// FuseRRF combines two rank-ordered ID lists (best match first) into a
// single fused ranking by reciprocal rank fusion, so a chunk that scores
// well on either keyword or vector similarity — not only one it tops both
// on — surfaces near the top.
func FuseRRF(rankings ...[]int64) []int64 {
	scores := map[int64]float64{}
	for _, ranking := range rankings {
		for pos, id := range ranking {
			scores[id] += 1.0 / (rrfK + float64(pos+1))
		}
	}
	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // stable, deterministic tie-break
	})
	return ids
}
