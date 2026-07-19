package retrievaleval

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/enqack/cognosis/internal/embed"
)

func TestSynthImplementsProvider(t *testing.T) {
	var _ embed.Provider = NewSynth(768, 40, 1, 0.5)
}

// TestSynthIsNonDegenerate is the guard Phase 0 earned the hard way: a
// generator whose randomness is accidentally shared across rows produces a
// corpus of identical vectors, on which every retrieval measurement is
// meaningless but plausible-looking.
func TestSynthIsNonDegenerate(t *testing.T) {
	s := NewSynth(768, 40, 7, DefaultSpread)
	texts := make([]string, 2000)
	for i := range texts {
		texts[i] = fmt.Sprintf("doc-%d", i)
	}
	vecs, err := s.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if got := DistinctVectors(vecs); got != len(texts) {
		t.Fatalf("degenerate corpus: %d distinct vectors from %d texts", got, len(texts))
	}
	// Determinism: the Provider contract is that identical text embeds
	// identically across calls.
	again, err := s.Embed(context.Background(), texts[:10])
	if err != nil {
		t.Fatal(err)
	}
	for i := range again {
		for j := range again[i] {
			if again[i][j] != vecs[i][j] {
				t.Fatalf("non-deterministic embedding for %q at component %d", texts[i], j)
			}
		}
	}
	// Unit norm, so cosine distance is 1-dot.
	for i, v := range vecs[:50] {
		var n float64
		for _, x := range v {
			n += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(n)-1) > 1e-5 {
			t.Fatalf("vector %d not unit norm: |v| = %v", i, math.Sqrt(n))
		}
	}
}

// TestSpreadCalibration records how discriminating each candidate Spread is.
// Not a pass/fail on the numbers -- it asserts only that DefaultSpread lands in
// the usable band, and logs the table so the choice is auditable rather than
// asserted.
func TestSpreadCalibration(t *testing.T) {
	// Spread is the ratio of noise norm to centre norm (see Synth.vec), so
	// the interesting band straddles 1.0 regardless of Dim.
	spreads := []float64{0.25, 0.5, 0.75, 1.0, 1.5, 2.0, 3.0, 5.0}
	reps := CalibrateSpread(768, 40, 7, spreads, 2000, 20, 50)

	t.Log("spread  top50_same_cluster  min_d   max_d   stddev_d")
	for _, r := range reps {
		t.Logf("%6.2f  %18.3f  %6.4f  %6.4f  %8.5f",
			r.Spread, r.TopKSameCluster, r.MinDist, r.MaxDist, r.StdDevDist)
	}

	// The usable band: exact top-50 must be neither entirely one cluster
	// (nothing to discriminate) nor washed out to chance (1/clusters = 0.025).
	var def *SpreadReport
	for i := range reps {
		if reps[i].Spread == DefaultSpread {
			def = &reps[i]
		}
	}
	if def == nil {
		t.Fatalf("DefaultSpread %v not among calibrated spreads %v", DefaultSpread, spreads)
	}
	if def.TopKSameCluster > 0.99 {
		t.Errorf("DefaultSpread %v too tight: top-50 is %.3f same-cluster, recall will read 1.0 at any ef_search",
			DefaultSpread, def.TopKSameCluster)
	}
	if def.TopKSameCluster < 0.10 {
		t.Errorf("DefaultSpread %v too wide: top-50 is %.3f same-cluster, approaching uniform-random",
			DefaultSpread, def.TopKSameCluster)
	}
	// The sharper guard. Uniform-random 768-dim vectors concentrate at
	// stddev ~0.036 (the spread=5.0 row); a corpus at that floor has no
	// exploitable structure and measures the adversarial case, not production.
	const uniformRandomFloor = 0.045
	if def.StdDevDist < uniformRandomFloor {
		t.Errorf("DefaultSpread %v distance stddev %.5f is at the uniform-random floor: "+
			"corpus has washed out to unstructured", DefaultSpread, def.StdDevDist)
	}
	// Nearest neighbours must be meaningfully nearer than the bulk, or there
	// is no neighbourhood for an ANN index to find.
	if def.MinDist > 0.7 {
		t.Errorf("DefaultSpread %v: nearest neighbour at distance %.4f, too far to constitute a cluster",
			DefaultSpread, def.MinDist)
	}
}
