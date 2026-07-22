package retrievaleval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/query"
)

// What does the fan-effect diversity penalty buy, and at what cost to relevance?
//
// Fusion is chunk-level with no per-note constraint, so one long note's chunks
// can crowd the top-K while a shorter relevant note never places (observed on
// the live vault: 5 of 12 results from one note). The penalty scales a note's
// n-th chunk by decay^n. This sweep runs the ladder through the real engine
// (Tuning.DiversityDecay) and reports, against the off arm:
//   - SOURCES@8: distinct notes in the top-8 -- what the penalty is FOR; higher
//     is more diverse.
//   - TOPK-REL: fraction of the top-8 in the query's own cluster. Unlike the
//     graph-weight sweep, the corpus carries cluster labels, so here we CAN tell
//     "more diverse" from "worse": diversity must not collapse relevance.
//   - RBO/JACCARD/CHANGED vs off.
//
// Broken-premise only: if the off arm never crowds (every top-8 already spans 8
// notes), there is nothing to diversify and the sweep is measuring noise.
func TestDiversityRerankSweep(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)
	opts := query.Options{}

	arms := []struct {
		name   string
		tuning query.Tuning
	}{
		{"off", query.Tuning{DiversityDecay: -1}},
		{"decay=0.75", query.Tuning{DiversityDecay: 0.75}},
		{"decay=0.5", query.Tuning{DiversityDecay: 0.5}},
		{"decay=0.25", query.Tuning{DiversityDecay: 0.25}},
		{"best-only", query.Tuning{DiversityDecay: 0.05}},
	}

	// Off-arm baseline, and the crowding premise.
	off := make([][]query.Result, len(c.Queries))
	crowdedQueries := 0
	for i, q := range c.Queries {
		c.Engine.Tuning = query.Tuning{DiversityDecay: -1}
		res, err := c.Engine.Run(ctx, q.Text, opts)
		if err != nil {
			t.Fatal(err)
		}
		off[i] = res
		top := capK(res)
		if distinctNotes(top) < len(top) {
			crowdedQueries++
		}
	}
	if crowdedQueries == 0 {
		t.Fatalf("no query crowds in the off arm: every top-%d already spans %d distinct notes, "+
			"so there is nothing for the diversity penalty to do -- the corpus, not the penalty, is the suspect",
			query.DefaultTopK, query.DefaultTopK)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "diversity re-rank sweep -- %d chunks, %d queries, %d/%d off-arm queries crowd\n\n",
		spec.Notes*spec.ChunksPerNote, len(c.Queries), crowdedQueries, len(c.Queries))
	b.WriteString("SOURCES@8 = mean distinct notes in the top-8 (higher = more diverse). TOPK-REL =\n" +
		"mean fraction of the top-8 in the query's cluster (the relevance guard -- must hold).\n" +
		"RBO/JACCARD/CHANGED are vs the off arm.\n\n")
	fmt.Fprintf(&b, "%-12s %10s %9s %8s %8s %9s\n", "ARM", "SOURCES@8", "TOPK-REL", "RBO", "JACCARD", "CHANGED")

	for _, arm := range arms {
		c.Engine.Tuning = arm.tuning
		var sumSources, sumRel, sumRBO, sumJac float64
		changed := 0
		for i, q := range c.Queries {
			res := off[i]
			if arm.name != "off" {
				var err error
				res, err = c.Engine.Run(ctx, q.Text, opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			top := capK(res)
			sumSources += float64(distinctNotes(top))
			rel := 0
			for _, r := range top {
				if c.Provider.Labels[r.Content] == q.Cluster {
					rel++
				}
			}
			if len(top) > 0 {
				sumRel += float64(rel) / float64(len(top))
			}
			rbo, jac := Overlap(off[i], res, query.DefaultTopK)
			sumRBO += rbo
			sumJac += jac
			if jac < 1 {
				changed++
			}
		}
		n := float64(len(c.Queries))
		fmt.Fprintf(&b, "%-12s %10.2f %9.3f %8.3f %8.3f %7d/%-2d\n",
			arm.name, sumSources/n, sumRel/n, sumRBO/n, sumJac/n, changed, len(c.Queries))
	}
	c.Engine.Tuning = query.Tuning{}

	writeArtifact(t, "diversity_rerank_sweep.txt", b.String())
	t.Log("\n" + b.String())
}

// capK returns the fused top-K slice a caller compares against: DefaultTopK is
// the only cap any sweep uses, so it is not a parameter.
func capK(res []query.Result) []query.Result {
	if len(res) > query.DefaultTopK {
		return res[:query.DefaultTopK]
	}
	return res
}

func distinctNotes(res []query.Result) int {
	seen := map[string]bool{}
	for _, r := range res {
		seen[r.Path] = true
	}
	return len(seen)
}
