package retrievaleval

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Q2: recall of the vector leg against brute-force exact KNN over an
// identical filter scope.
//
// No relevance judgments are involved. The vector leg's contract is "the k
// nearest by cosine", so exact KNN is the correct ground truth rather than a
// proxy, and divergence from it is by definition the defect.
func TestVectorLegRecallVsExact(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)

	var b strings.Builder
	fmt.Fprintf(&b, "vector leg recall vs exact KNN -- %d chunks, k=50, averaged over %d queries\n\n",
		spec.Notes*spec.ChunksPerNote, len(c.Queries))
	fmt.Fprintf(&b, "%-14s %-24s %8s %8s %8s %8s\n",
		"SCOPE", "SETTING", "RETURNED", "RECALL", "NDCG", "KENDALL")

	recall := map[string]map[string]float64{}
	hnswSeen := map[string]bool{}

	for _, scope := range c.ScopeNames() {
		filter := c.Scopes()[scope]
		recall[scope] = map[string]float64{}
		for _, gs := range gucSettings {
			var sumRecall, sumNDCG, sumKendall float64
			var sumReturned int
			var plan string
			for _, q := range c.Queries {
				vec, err := c.Provider.EmbedQuery(ctx, q.Text)
				if err != nil {
					t.Fatal(err)
				}
				approx, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, 50, gs.Set, plan == "")
				if err != nil {
					t.Fatal(err)
				}
				if plan == "" {
					plan = approx.Plan
				}
				exact, err := c.Store.ProbeVectorExact(ctx, c.Table, vec, filter, 50)
				if err != nil {
					t.Fatal(err)
				}
				// Ground truth is only ground truth if it bypassed the index.
				if usedHNSW(exact.Plan) {
					t.Fatalf("exact probe used the HNSW index; not ground truth:\n%s", exact.Plan)
				}
				ag := Agree(approx.Rows, exact.Rows, 50)
				sumRecall += ag.Recall
				sumNDCG += ag.NDCG
				sumKendall += ag.Kendall
				sumReturned += len(approx.Rows)
			}
			n := float64(len(c.Queries))
			recall[scope][gs.Name] = sumRecall / n
			if usedHNSW(plan) {
				hnswSeen[scope] = true
			}
			fmt.Fprintf(&b, "%-14s %-24s %8d %8.3f %8.3f %8.3f\n",
				scope, gs.Name, sumReturned/len(c.Queries),
				sumRecall/n, sumNDCG/n, sumKendall/n)
		}
	}
	writeArtifact(t, "vector_recall.txt", b.String())
	t.Log("\n" + b.String())

	if len(hnswSeen) == 0 {
		t.Fatalf("no scope used the HNSW index; nothing about ANN recall was measured")
	}

	// Relation, not magnitude. Per-query HNSW recall is not monotone in
	// ef_search, but averaged over the query set it must not get worse.
	const tolerance = 0.02
	for scope := range hnswSeen {
		def := recall[scope][baselineSetting]
		// Derived from gucSettings rather than hardcoded: a duplicated name
		// list silently reads a missing map key as 0.0 and fires a false failure.
		for _, gs := range gucSettings {
			if gs.Name == baselineSetting {
				continue
			}
			if got := recall[scope][gs.Name]; got < def-tolerance {
				t.Errorf("%s: %s recall %.3f < pre-fix baseline %.3f (tolerance %.2f); "+
					"a larger candidate list must not reduce recall", scope, gs.Name, got, def, tolerance)
			}
		}
		// Sanity floor: a scope using HNSW must retrieve *something* correct,
		// or the corpus geometry is degenerate and the whole run is void.
		if def <= 0 {
			t.Errorf("%s: pre-fix baseline recall is %.3f -- corpus geometry is degenerate", scope, def)
		}
	}
}
