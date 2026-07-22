package retrievaleval

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// Manual: graph-weight sweep against a dump of the REAL vault, where the link
// graph has the clustered topology the synthetic corpus cannot reproduce (there
// the weight is inert -- GRAPH-ONLY@8 = 0 at every value). Gated on
// COGNOSIS_GRAPHTUNE_DSN (an isolated dump, never the live DB); skipped in CI.
// Query set is the notes' own summaries -- real text over the real topology.
func TestGraphWeightRealVault(t *testing.T) {
	rv := realVaultSetup(t)
	ctx, s, prov, e := rv.ctx, rv.s, rv.prov, rv.e
	table := ollamaTable
	queries := rv.summaryQueries(t)

	const poolN = 50
	filter := store.Filter{}
	opts := query.Options{}

	// Probe reconstruction per query (mirrors Run's leg assembly incl. the @2 OR
	// fallback) to classify graph-only candidates.
	type probeSets struct{ graphOnly map[string]bool }
	probes := make([]probeSets, len(queries))
	graphContributed := 0
	for i, q := range queries {
		vec, err := prov.EmbedQuery(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		vp, err := s.ProbeVector(ctx, table, vec, filter, poolN, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := s.ProbeFTSMode(ctx, q, store.TSQueryWebsearch, filter, poolN, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		ftsRows := fp.Rows
		if len(ftsRows) < 2 {
			if op, err := s.ProbeFTSNoteLevel(ctx, q, filter, poolN, nil, false); err == nil && len(op.Rows) > len(ftsRows) {
				ftsRows = op.Rows
			}
		}
		seen := map[uuid.UUID]bool{}
		var seeds []uuid.UUID
		for _, rs := range [][]store.RankedChunk{vp.Rows, ftsRows} {
			for _, r := range rs {
				if !seen[r.NoteID] {
					seen[r.NoteID] = true
					seeds = append(seeds, r.NoteID)
				}
			}
		}
		gp, err := s.ProbeGraph(ctx, seeds, filter, poolN, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(gp.Rows) > 0 {
			graphContributed++
		}
		vset, fset := contentSet(vp.Rows), contentSet(ftsRows)
		go2 := map[string]bool{}
		for _, r := range gp.Rows {
			if !vset[r.Content] && !fset[r.Content] {
				go2[r.Content] = true
			}
		}
		probes[i] = probeSets{graphOnly: go2}
	}

	// Shipped (w=0.5) top-8 per query, the comparison baseline.
	shipped := make([][]query.Result, len(queries))
	for i, q := range queries {
		e.Tuning = query.Tuning{}
		res, err := e.Run(ctx, q, opts)
		if err != nil {
			t.Fatal(err)
		}
		shipped[i] = res
	}

	arms := []struct {
		name   string
		tuning query.Tuning
	}{
		{"DisableGraph", query.Tuning{DisableGraph: true}},
		{"w=0", query.Tuning{GraphWeight: -1}},
		{"w=0.25", query.Tuning{GraphWeight: 0.25}},
		{"SHIPPED(w=0.5)", query.Tuning{}},
		{"w=0.75", query.Tuning{GraphWeight: 0.75}},
		{"w=1.0", query.Tuning{GraphWeight: 1}},
	}

	var b strings.Builder
	fmt.Fprintf(&b, "graph-leg weight sweep -- REAL vault dump, %d notes queried by summary\n", len(queries))
	fmt.Fprintf(&b, "graph leg contributed candidates on %d/%d queries\n\n", graphContributed, len(queries))
	b.WriteString("RBO/JACCARD/CHANGED vs shipped w=0.5. GRAPH-ONLY@8 = mean top-8 members no\n" +
		"other leg surfaced (what the weight is for); DISPLACED = mean shipped top-8\n" +
		"members pushed out. Real link topology, unlike the synthetic corpus.\n\n")
	fmt.Fprintf(&b, "%-16s %8s %8s %9s %13s %10s\n", "ARM", "RBO", "JACCARD", "CHANGED", "GRAPH-ONLY@8", "DISPLACED")

	for _, arm := range arms {
		e.Tuning = arm.tuning
		var sumRBO, sumJac, sumGraphOnly, sumDisplaced float64
		changed := 0
		for i, q := range queries {
			res := shipped[i]
			if arm.name != "SHIPPED(w=0.5)" {
				var err error
				res, err = e.Run(ctx, q, opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			rbo, jac := Overlap(shipped[i], res, query.DefaultTopK)
			sumRBO += rbo
			sumJac += jac
			if jac < 1 {
				changed++
			}
			top := res
			if len(top) > query.DefaultTopK {
				top = top[:query.DefaultTopK]
			}
			topSet := map[string]bool{}
			for _, r := range top {
				topSet[r.Content] = true
				if probes[i].graphOnly[r.Content] {
					sumGraphOnly++
				}
			}
			base := shipped[i]
			if len(base) > query.DefaultTopK {
				base = base[:query.DefaultTopK]
			}
			for _, r := range base {
				if !topSet[r.Content] {
					sumDisplaced++
				}
			}
		}
		n := float64(len(queries))
		fmt.Fprintf(&b, "%-16s %8.3f %8.3f %7d/%-2d %13.2f %10.2f\n",
			arm.name, sumRBO/n, sumJac/n, changed, len(queries), sumGraphOnly/n, sumDisplaced/n)
	}
	e.Tuning = query.Tuning{}
	t.Log("\n" + b.String())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
