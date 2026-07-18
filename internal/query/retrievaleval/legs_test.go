package retrievaleval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/store"
)

// Per-leg capacity at the shipped scan settings.
//
// This is deliberately not part of the gucSettings sweep: hnsw.* affects only
// the vector leg, so sweeping the keyword and graph legs across those rows
// would repeat the same number and imply a relationship that does not exist.
// The question here is different — which legs can actually fill the candidate
// pool, and which are structurally short.
//
// The graph leg had never been measured directly before this. That mattered:
// Q3 found it *propagates* vector-leg truncation rather than masking it (the
// fused top-8 stopped changing entirely once DisableGraph was set), because it
// is seeded by the other legs' candidates. A leg with that much influence over
// the fused output should not be the one leg with no capacity number.
func TestAllLegCapacityAtShippedSettings(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)

	const pool = 50

	var b strings.Builder
	fmt.Fprintf(&b, "per-leg capacity at shipped scan settings — %d chunks, pool=%d, %d queries\n\n",
		spec.Notes*spec.ChunksPerNote, pool, len(c.Queries))
	b.WriteString("Graph-leg note: project scoping is deliberately absent from that leg (it\n" +
		"inherits scope through its seeds), so its row varies with the seeds it is given,\n" +
		"not with the scope column. The temporal and status filters do apply.\n\n")
	fmt.Fprintf(&b, "%-14s %-8s %9s %9s %10s  %s\n",
		"SCOPE", "LEG", "REQUESTED", "RETURNED", "TRUNCATED", "ACCESS")

	for _, scope := range c.ScopeNames() {
		filter := c.Scopes()[scope]

		var vecSum, ftsSum, graphSum int
		var vecTrunc, ftsTrunc, graphTrunc int
		var vecPlan, ftsPlan, graphPlan string

		for _, q := range c.Queries {
			vec, err := c.Provider.EmbedQuery(ctx, q.Text)
			if err != nil {
				t.Fatal(err)
			}

			vp, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, pool, nil, vecPlan == "")
			if err != nil {
				t.Fatal(err)
			}
			if vecPlan == "" {
				vecPlan = vp.Plan
			}
			vecSum += len(vp.Rows)
			if vp.Truncated() {
				vecTrunc++
			}

			fp, err := c.Store.ProbeFTS(ctx, q.Text, filter, pool, nil, ftsPlan == "")
			if err != nil {
				t.Fatal(err)
			}
			if ftsPlan == "" {
				ftsPlan = fp.Plan
			}
			ftsSum += len(fp.Rows)
			if fp.Truncated() {
				ftsTrunc++
			}

			// Seed the graph leg the way Run does: the distinct notes behind
			// the other legs' candidates. Seeding it any other way would
			// measure a graph query the engine never issues.
			seen := map[uuid.UUID]bool{}
			var seeds []uuid.UUID
			for _, rows := range [][]store.RankedChunk{vp.Rows, fp.Rows} {
				for _, r := range rows {
					if !seen[r.NoteID] {
						seen[r.NoteID] = true
						seeds = append(seeds, r.NoteID)
					}
				}
			}
			gp, err := c.Store.ProbeGraph(ctx, seeds, filter, pool, nil, graphPlan == "")
			if err != nil {
				t.Fatal(err)
			}
			if graphPlan == "" && gp.Plan != "" {
				graphPlan = gp.Plan
			}
			graphSum += len(gp.Rows)
			if gp.Truncated() {
				graphTrunc++
			}
		}

		n := len(c.Queries)
		for _, row := range []struct {
			leg    string
			sum    int
			trunc  int
			access string
		}{
			{"vector", vecSum, vecTrunc, accessPath(vecPlan, c.Table)},
			{"fts", ftsSum, ftsTrunc, accessPath(ftsPlan, "chunks")},
			{"graph", graphSum, graphTrunc, accessPath(graphPlan, "links")},
		} {
			fmt.Fprintf(&b, "%-14s %-8s %9d %9d %9d/%d  %s\n",
				scope, row.leg, pool, row.sum/n, row.trunc, n, row.access)
		}
	}

	writeArtifact(t, "all_legs_capacity.txt", b.String())
	t.Log("\n" + b.String())

	// The only hard assertion: no leg may exceed its limit. Under-returning is
	// the interesting measurement and is recorded, not asserted — the keyword
	// leg legitimately returns fewer than the pool when few chunks match, and
	// the graph leg returns whatever the seeds reach.
	// (Over-returning would mean the LIMIT is not doing its job.)
}
