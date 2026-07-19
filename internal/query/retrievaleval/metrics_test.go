package retrievaleval

import (
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

func chunks(ids ...uuid.UUID) []store.RankedChunk {
	out := make([]store.RankedChunk, len(ids))
	for i, id := range ids {
		out[i] = store.RankedChunk{ChunkID: id}
	}
	return out
}

func TestAgreeIdenticalIsPerfect(t *testing.T) {
	ids := make([]uuid.UUID, 10)
	for i := range ids {
		ids[i] = uuid.New()
	}
	got := Agree(chunks(ids...), chunks(ids...), 10)
	if got.Recall != 1 {
		t.Errorf("recall = %v, want 1", got.Recall)
	}
	if math.Abs(got.NDCG-1) > 1e-9 {
		t.Errorf("nDCG = %v, want 1", got.NDCG)
	}
	if math.Abs(got.Kendall-1) > 1e-9 {
		t.Errorf("kendall = %v, want 1", got.Kendall)
	}
}

func TestAgreeDisjointIsZero(t *testing.T) {
	a := make([]uuid.UUID, 5)
	e := make([]uuid.UUID, 5)
	for i := range a {
		a[i], e[i] = uuid.New(), uuid.New()
	}
	got := Agree(chunks(a...), chunks(e...), 5)
	if got.Recall != 0 || got.NDCG != 0 {
		t.Errorf("disjoint lists: recall=%v nDCG=%v, want 0/0", got.Recall, got.NDCG)
	}
}

// A truncated leg is the exact failure mode under investigation: the right
// items, but too few of them.
func TestAgreeTruncatedLeg(t *testing.T) {
	ids := make([]uuid.UUID, 50)
	for i := range ids {
		ids[i] = uuid.New()
	}
	got := Agree(chunks(ids[:40]...), chunks(ids...), 50)
	if math.Abs(got.Recall-0.8) > 1e-9 {
		t.Errorf("recall = %v, want 0.8 (40 of 50)", got.Recall)
	}
	if got.NDCG >= 1 {
		t.Errorf("nDCG = %v, want < 1 for a truncated list", got.NDCG)
	}
	// Order of what *was* returned is still correct.
	if math.Abs(got.Kendall-1) > 1e-9 {
		t.Errorf("kendall = %v, want 1 (returned items are correctly ordered)", got.Kendall)
	}
}

// nDCG must punish position, not just membership -- the property Recall can't see.
func TestNDCGIsPositionSensitive(t *testing.T) {
	ids := make([]uuid.UUID, 10)
	for i := range ids {
		ids[i] = uuid.New()
	}
	reversed := make([]uuid.UUID, 10)
	for i := range ids {
		reversed[i] = ids[len(ids)-1-i]
	}
	same := Agree(chunks(ids...), chunks(ids...), 10)
	rev := Agree(chunks(reversed...), chunks(ids...), 10)
	if rev.Recall != same.Recall {
		t.Fatalf("recall should be identical (same set): %v vs %v", rev.Recall, same.Recall)
	}
	if rev.NDCG >= same.NDCG {
		t.Errorf("reversed nDCG %v should be < in-order %v", rev.NDCG, same.NDCG)
	}
	if rev.Kendall >= 0 {
		t.Errorf("reversed kendall = %v, want negative", rev.Kendall)
	}
}

func results(paths ...string) []query.Result {
	out := make([]query.Result, len(paths))
	for i, p := range paths {
		out[i] = query.Result{Path: p}
	}
	return out
}

func TestOverlapIdenticalAndDisjoint(t *testing.T) {
	a := results("a", "b", "c")
	if rbo, jac := Overlap(a, a, 3); math.Abs(rbo-1) > 1e-9 || math.Abs(jac-1) > 1e-9 {
		t.Errorf("identical: rbo=%v jaccard=%v, want 1/1", rbo, jac)
	}
	b := results("x", "y", "z")
	if rbo, jac := Overlap(a, b, 3); rbo != 0 || jac != 0 {
		t.Errorf("disjoint: rbo=%v jaccard=%v, want 0/0", rbo, jac)
	}
}

// The two measures answer different questions: same set, different order.
func TestOverlapRBODistinguishesOrder(t *testing.T) {
	a := results("a", "b", "c")
	b := results("c", "b", "a")
	rbo, jac := Overlap(a, b, 3)
	if math.Abs(jac-1) > 1e-9 {
		t.Errorf("jaccard = %v, want 1 (identical membership)", jac)
	}
	if rbo >= 1 {
		t.Errorf("rbo = %v, want < 1 (order differs)", rbo)
	}
}

// RBO must weight the head: a swap at rank 1 should cost more than at rank 8.
func TestOverlapRBOIsTopWeighted(t *testing.T) {
	base := results("a", "b", "c", "d", "e", "f", "g", "h")
	headSwap := results("b", "a", "c", "d", "e", "f", "g", "h")
	tailSwap := results("a", "b", "c", "d", "e", "f", "h", "g")
	headRBO, _ := Overlap(base, headSwap, 8)
	tailRBO, _ := Overlap(base, tailSwap, 8)
	if headRBO >= tailRBO {
		t.Errorf("head swap rbo %v should be < tail swap rbo %v", headRBO, tailRBO)
	}
}
