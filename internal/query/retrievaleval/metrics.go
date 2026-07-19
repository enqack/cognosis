package retrievaleval

import (
	"math"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// Agreement scores an approximate result list against exact ground truth.
//
// No human relevance judgments are involved, and none are needed: the vector
// leg's contract is literally "the k nearest by cosine", so brute-force KNN is
// the *correct* ground truth rather than a stand-in for it, and any divergence
// is by definition the defect being measured.
type Agreement struct {
	K int
	// Recall is |intersection(approx, exact)| / |exact| over the top K. The
	// headline number.
	Recall float64
	// NDCG grades by the item's rank in the exact list, so retrieving the true
	// #1 at position 40 scores worse than retrieving it at position 2 -- a
	// distinction Recall alone cannot see.
	NDCG float64
	// Kendall is rank correlation over the items present in both lists. It
	// isolates ordering quality from membership: a leg can retrieve exactly
	// the right set in the wrong order, which RRF will then fuse wrongly.
	Kendall float64
}

// Agree compares an approximate leg result against exact ground truth.
func Agree(approx, exact []store.RankedChunk, k int) Agreement {
	a := truncIDs(approx, k)
	e := truncIDs(exact, k)
	out := Agreement{K: k}
	if len(e) == 0 {
		return out
	}

	exactRank := make(map[uuid.UUID]int, len(e))
	for i, id := range e {
		exactRank[id] = i
	}

	var hits int
	for _, id := range a {
		if _, ok := exactRank[id]; ok {
			hits++
		}
	}
	out.Recall = float64(hits) / float64(len(e))

	// nDCG with graded gain from the exact rank: gain = len(e) - exactRank.
	var dcg float64
	for i, id := range a {
		r, ok := exactRank[id]
		if !ok {
			continue
		}
		gain := float64(len(e) - r)
		dcg += gain / math.Log2(float64(i)+2)
	}
	var idcg float64
	for i := range e {
		idcg += float64(len(e)-i) / math.Log2(float64(i)+2)
	}
	if idcg > 0 {
		out.NDCG = dcg / idcg
	}

	out.Kendall = kendallTau(a, exactRank)
	return out
}

// kendallTau is rank correlation between the approximate ordering and the
// exact ranks, over items appearing in both.
func kendallTau(approx []uuid.UUID, exactRank map[uuid.UUID]int) float64 {
	var common []int
	for _, id := range approx {
		if r, ok := exactRank[id]; ok {
			common = append(common, r)
		}
	}
	if len(common) < 2 {
		return 0
	}
	var concordant, discordant int
	for i := range common {
		for j := i + 1; j < len(common); j++ {
			switch {
			case common[i] < common[j]:
				concordant++
			case common[i] > common[j]:
				discordant++
			}
		}
	}
	total := concordant + discordant
	if total == 0 {
		return 0
	}
	return float64(concordant-discordant) / float64(total)
}

// Overlap compares two fused result sets against each other -- the Q3 question,
// "does correcting the scan settings change what the agent actually sees".
//
// rbo is rank-biased overlap (p=0.9), which weights the head of the list far
// more than the tail: for a top-8 context injection, a change at position 1
// matters and a change at position 8 barely does. jaccard is plain set
// overlap, reported alongside because the two answer different questions --
// identical membership in a different order gives jaccard 1.0 and rbo < 1.0.
func Overlap(a, b []query.Result, k int) (rbo, jaccard float64) {
	x := truncPaths(a, k)
	y := truncPaths(b, k)
	if len(x) == 0 && len(y) == 0 {
		return 1, 1
	}

	const p = 0.9
	seenX := map[string]bool{}
	seenY := map[string]bool{}
	var sum, weight float64
	depth := max(len(x), len(y))
	for d := range depth {
		if d < len(x) {
			seenX[x[d]] = true
		}
		if d < len(y) {
			seenY[y[d]] = true
		}
		var inter int
		for kk := range seenX {
			if seenY[kk] {
				inter++
			}
		}
		w := math.Pow(p, float64(d))
		sum += w * float64(inter) / float64(d+1)
		weight += w
	}
	if weight > 0 {
		rbo = sum / weight
	}

	setX := map[string]bool{}
	for _, s := range x {
		setX[s] = true
	}
	var inter, union int
	setY := map[string]bool{}
	for _, s := range y {
		setY[s] = true
	}
	for s := range setX {
		if setY[s] {
			inter++
		}
	}
	union = len(setX) + len(setY) - inter
	if union > 0 {
		jaccard = float64(inter) / float64(union)
	}
	return rbo, jaccard
}

// ClusterPrecision is the fraction of the top-k whose chunk shares the query's
// latent cluster.
//
// Unlike Agree, this is a *pseudo*-label, and the difference matters. It
// exists to score rankers for which brute-force KNN cannot serve as ground
// truth -- principally the keyword leg, where there is no free notion of "the
// correct answer". It will separate a broken ranker from a working one. It
// will not credibly rank two decent rankers against each other, because the
// corpus's topics were generated to be lexically separable in the first place.
// Treat a ts_rank_cd-vs-BM25 comparison built on this as a smoke test, not as
// evidence.
func ClusterPrecision(c *Corpus, q EvalQuery, rs []query.Result, k int) float64 {
	if k > len(rs) {
		k = len(rs)
	}
	if k == 0 {
		return 0
	}
	var same int
	for _, r := range rs[:k] {
		if c.Provider.ClusterOf(r.Content) == q.Cluster {
			same++
		}
	}
	return float64(same) / float64(k)
}

func truncIDs(rs []store.RankedChunk, k int) []uuid.UUID {
	if k > len(rs) {
		k = len(rs)
	}
	out := make([]uuid.UUID, k)
	for i := range k {
		out[i] = rs[i].ChunkID
	}
	return out
}

func truncPaths(rs []query.Result, k int) []string {
	if k > len(rs) {
		k = len(rs)
	}
	out := make([]string, k)
	for i := range k {
		// Path plus heading identifies a chunk in the fused output; Result
		// carries no chunk id.
		out[i] = rs[i].Path + "#" + rs[i].HeadingPath
	}
	return out
}
