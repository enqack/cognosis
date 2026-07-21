package retrievaleval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// What does the graph leg add that no other leg can, and how does that scale
// with link density and corpus size?
//
// The real-traffic finding this quantifies: on the young production vault the
// graph leg added zero unique candidates (everything it surfaced, the vector
// leg already had); on the mature vault it expanded the candidate pool by a
// median of ~40% with chunks no text or vector match would have found. That
// trajectory is the leg's whole justification, and the synthetic ladder is
// the only place it can be confirmed cleanly -- the production vault is too
// small to reach the mature regime.
//
// The test records one cell: the corpus size and LinkDegree it was run at
// (both env-tunable, COGNOSIS_EVAL_NOTES and COGNOSIS_EVAL_LINKDEGREE). The
// ladder is produced by running it repeatedly, exactly as the size baseline
// runs the other sweeps. The artifact header carries both parameters so a
// collected set of cells is self-describing.
//
// Like the other sweeps it asserts broken premises only: no threshold on the
// contribution itself, because zero unique candidates at low link degree is
// the young-vault regime, not a defect.
func TestGraphLegContribution(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)

	const (
		pool        = 50
		topK        = 8
		rrfK        = 60
		graphWeight = 0.5
	)
	filter := store.Filter{}

	if spec.LinkDegree <= 0 {
		t.Fatalf("spec.LinkDegree = %d: a linkless corpus cannot measure the graph leg "+
			"(set COGNOSIS_EVAL_LINKDEGREE >= 1)", spec.LinkDegree)
	}

	type row struct {
		others      int // distinct chunks the vector+keyword legs surfaced
		graph       int // graph-leg candidates
		unique      int // graph candidates neither other leg had
		uniqueInTop int // of those, how many survived into the fused top-K
	}
	rows := make([]row, 0, len(c.Queries))
	graphAny, uniqueAny := 0, 0

	for _, q := range c.Queries {
		vec, err := c.Provider.EmbedQuery(ctx, q.Text)
		if err != nil {
			t.Fatal(err)
		}
		vp, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := c.Store.ProbeFTSMode(ctx, q.Text, store.TSQueryWebsearch, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		ftsRows := fp.Rows
		// Mirror the shipped @2 OR fallback, as the graph-weight sweep does:
		// the seeds the graph leg receives in production are post-fallback.
		if len(ftsRows) < 2 {
			op, err := c.Store.ProbeFTSMode(ctx, q.Text, store.TSQueryOr, filter, pool, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(op.Rows) > len(ftsRows) {
				ftsRows = op.Rows
			}
		}

		others := map[uuid.UUID]bool{}
		seen := map[uuid.UUID]bool{}
		var seeds []uuid.UUID
		for _, rs := range [][]store.RankedChunk{vp.Rows, ftsRows} {
			for _, r := range rs {
				others[r.ChunkID] = true
				if !seen[r.NoteID] {
					seen[r.NoteID] = true
					seeds = append(seeds, r.NoteID)
				}
			}
		}
		gp, err := c.Store.ProbeGraph(ctx, seeds, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		uniqueSet := map[uuid.UUID]bool{}
		for _, r := range gp.Rows {
			if !others[r.ChunkID] {
				uniqueSet[r.ChunkID] = true
			}
		}

		fused := query.FuseRRF(rrfK, func(ch store.RankedChunk) uuid.UUID { return ch.ChunkID },
			[]query.Leg[store.RankedChunk]{
				{Items: vp.Rows, Weight: 1},
				{Items: ftsRows, Weight: 1},
				{Items: gp.Rows, Weight: graphWeight},
			})
		if len(fused) > topK {
			fused = fused[:topK]
		}
		uniqueInTop := 0
		for _, f := range fused {
			if uniqueSet[f.Item.ChunkID] {
				uniqueInTop++
			}
		}

		r := row{others: len(others), graph: len(gp.Rows), unique: len(uniqueSet), uniqueInTop: uniqueInTop}
		rows = append(rows, r)
		if r.graph > 0 {
			graphAny++
		}
		if r.unique > 0 {
			uniqueAny++
		}
	}

	// Premise: with LinkDegree >= 1 the link graph must reach *something* from
	// the seeds. Zero graph candidates everywhere means the links were never
	// planted (fixture defect), not that the leg is worthless.
	if graphAny == 0 {
		t.Fatalf("graph leg returned 0 candidates on all %d queries at LinkDegree=%d: "+
			"the corpus link pass did not produce a traversable graph", len(c.Queries), spec.LinkDegree)
	}

	var sumOthers, sumGraph, sumUnique, sumUniqueTop int
	uniques := make([]int, len(rows))
	for i, r := range rows {
		sumOthers += r.others
		sumGraph += r.graph
		sumUnique += r.unique
		sumUniqueTop += r.uniqueInTop
		uniques[i] = r.unique
	}
	sort.Ints(uniques)
	n := float64(len(rows))
	expansion := 0.0
	if sumOthers > 0 {
		expansion = float64(sumUnique) / float64(sumOthers)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "graph-leg unique contribution -- %d chunks (%d notes), LinkDegree=%d, %d queries\n",
		spec.Notes*spec.ChunksPerNote, spec.Notes, spec.LinkDegree, len(c.Queries))
	fmt.Fprintf(&b, "pool=%d per leg, shipped fusion (rrfK=%d, graph weight %.2f), top-%d\n\n",
		pool, rrfK, graphWeight, topK)
	b.WriteString("OTHERS is the distinct chunks the vector+keyword legs surfaced; UNIQUE is\n" +
		"graph candidates neither had -- the notes only the link graph can reach.\n" +
		"EXPANSION is unique/others over the whole run. UNIQUE@8 is how many unique\n" +
		"candidates survived fusion into the top-8 -- the pool expanding without the\n" +
		"top-K moving is membership without consequence.\n\n")
	fmt.Fprintf(&b, "%-24s %10s\n", "METRIC", "VALUE")
	fmt.Fprintf(&b, "%-24s %10.1f\n", "mean others", float64(sumOthers)/n)
	fmt.Fprintf(&b, "%-24s %10.1f\n", "mean graph candidates", float64(sumGraph)/n)
	fmt.Fprintf(&b, "%-24s %10.1f\n", "mean unique", float64(sumUnique)/n)
	fmt.Fprintf(&b, "%-24s %10d\n", "median unique", uniques[len(uniques)/2])
	fmt.Fprintf(&b, "%-24s %9.1f%%\n", "pool expansion", expansion*100)
	fmt.Fprintf(&b, "%-24s %7d/%-2d\n", "queries w/ graph cands", graphAny, len(rows))
	fmt.Fprintf(&b, "%-24s %7d/%-2d\n", "queries w/ unique cands", uniqueAny, len(rows))
	fmt.Fprintf(&b, "%-24s %10d\n", "unique in top-8 (total)", sumUniqueTop)
	fmt.Fprintf(&b, "%-24s %10.2f\n", "unique in top-8 (mean)", float64(sumUniqueTop)/n)

	writeArtifact(t, "graph_leg_contribution.txt", b.String())
	t.Log("\n" + b.String())
}
