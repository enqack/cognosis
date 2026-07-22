package retrievaleval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// saNonGraph is one query's non-graph content sets (vector + keyword), used to
// decide GRAPH-ONLY@8: a top-8 member in neither set can only be a graph-leg
// chunk. See setupSpreadingActivation for why the keyword set must reconstruct
// Run's full AND-starvation fallback chain.
type saNonGraph struct{ vset, fset map[string]bool }

// saFixture is the shared, arm-independent state both spreading-activation
// sweeps run against: the engine, the summary query set, each query's non-graph
// content sets, and the shipped (depth 1, default) top-8 baseline.
type saFixture struct {
	ctx     context.Context
	e       *query.Engine
	queries []string
	ng      []saNonGraph
	shipped [][]query.Result
	opts    query.Options
}

// setupSpreadingActivation stands up the real-vault fixture shared by the
// depth x decay and depth x weight sweeps. Gated on COGNOSIS_GRAPHTUNE_DSN (an
// isolated dump, never the live DB); skipped in CI. Query set is the notes' own
// summaries -- real text over the real link topology.
//
// The non-graph reconstruction mirrors Run's leg assembly *exactly*, including
// the full AND-starvation fallback chain (note-level membership, then the bare
// OR floor). Getting the chain complete is what makes GRAPH-ONLY@8 robust: a
// top-8 member absent from both the vector and keyword sets can then only be a
// graph-leg chunk, with no depth- or weight-aware graph probe. Stopping at
// note-level mis-attributes OR-floor chunks to the graph leg and pins the
// shipped metric above 0.
func setupSpreadingActivation(t *testing.T) *saFixture {
	t.Helper()
	rv := realVaultSetup(t)
	ctx, s, prov, e := rv.ctx, rv.s, rv.prov, rv.e
	table := ollamaTable
	queries := rv.summaryQueries(t)

	const poolN = 50
	filter := store.Filter{}
	opts := query.Options{}

	ng := make([]saNonGraph, len(queries))
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
		// AND-starvation fallback chain, byte-identical to query.Engine: fire below
		// 2 candidates, take note-level then OR only when each strictly improves.
		const below = 2
		if len(ftsRows) < below {
			if op, err := s.ProbeFTSNoteLevel(ctx, q, filter, poolN, nil, false); err == nil && len(op.Rows) > len(ftsRows) {
				ftsRows = op.Rows
			}
			if len(ftsRows) < below {
				if op, err := s.ProbeFTSMode(ctx, q, store.TSQueryOr, filter, poolN, nil, false); err == nil && len(op.Rows) > len(ftsRows) {
					ftsRows = op.Rows
				}
			}
		}
		ng[i] = saNonGraph{vset: contentSet(vp.Rows), fset: contentSet(ftsRows)}
	}

	// Shipped (depth 1, default) top-8 per query -- the comparison baseline.
	shipped := make([][]query.Result, len(queries))
	for i, q := range queries {
		e.Tuning = query.Tuning{}
		res, err := e.Run(ctx, q, opts)
		if err != nil {
			t.Fatal(err)
		}
		shipped[i] = res
	}

	return &saFixture{ctx: ctx, e: e, queries: queries, ng: ng, shipped: shipped, opts: opts}
}

type saArm struct {
	name   string
	tuning query.Tuning
}

// runSweep evaluates each arm against the fixture and renders the artifact body.
// Metrics per arm, all vs the shipped depth-1 baseline: RBO and Jaccard over the
// top-K, count of queries whose top-K changed, GRAPH-ONLY@8 (mean top-8 members
// no other leg surfaced -- the key metric), and DISPLACED (mean shipped top-8
// members pushed out). An arm whose Tuning is the zero value reuses the
// precomputed shipped result rather than re-running Run.
func runSweep(t *testing.T, fx *saFixture, title, legend string, arms []saArm) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s -- REAL vault dump, %d notes queried by summary\n\n", title, len(fx.queries))
	fmt.Fprintf(&b, "%s\n\n", legend)
	fmt.Fprintf(&b, "%-16s %8s %8s %9s %13s %10s\n", "ARM", "RBO", "JACCARD", "CHANGED", "GRAPH-ONLY@8", "DISPLACED")

	for _, arm := range arms {
		fx.e.Tuning = arm.tuning
		var sumRBO, sumJac, sumGraphOnly, sumDisplaced float64
		changed := 0
		for i, q := range fx.queries {
			res := fx.shipped[i]
			if arm.tuning != (query.Tuning{}) {
				var err error
				res, err = fx.e.Run(fx.ctx, q, fx.opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			rbo, jac := Overlap(fx.shipped[i], res, query.DefaultTopK)
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
				if !fx.ng[i].vset[r.Content] && !fx.ng[i].fset[r.Content] {
					sumGraphOnly++
				}
			}
			base := fx.shipped[i]
			if len(base) > query.DefaultTopK {
				base = base[:query.DefaultTopK]
			}
			for _, r := range base {
				if !topSet[r.Content] {
					sumDisplaced++
				}
			}
		}
		n := float64(len(fx.queries))
		fmt.Fprintf(&b, "%-16s %8.3f %8.3f %7d/%-2d %13.2f %10.2f\n",
			arm.name, sumRBO/n, sumJac/n, changed, len(fx.queries), sumGraphOnly/n, sumDisplaced/n)
	}
	fx.e.Tuning = query.Tuning{}
	return b.String()
}

// TestSpreadingActivation sweeps the graph leg's hop depth {1,2} x per-hop decay
// {0.3,0.5,0.7} at the shipped graph weight. GRAPH-ONLY@8 is 0.00 at depth 1 by
// construction; whether depth 2 lifts it off zero is the question. Finding: it
// does not -- depth-2 reorders but injects no novel chunks at weight 0.5, because
// the hop-2 candidates rank too low under the decay discount to clear the cut.
// The recall lever is depth *and* weight together -- see TestSpreadingActivation
// DepthWeight.
func TestSpreadingActivation(t *testing.T) {
	fx := setupSpreadingActivation(t)
	arms := []saArm{
		{"SHIPPED(d1)", query.Tuning{}},
		{"d1,decay0.3", query.Tuning{GraphDepth: 1, GraphDecay: 0.3}},
		{"d1,decay0.5", query.Tuning{GraphDepth: 1, GraphDecay: 0.5}},
		{"d1,decay0.7", query.Tuning{GraphDepth: 1, GraphDecay: 0.7}},
		{"d2,decay0.3", query.Tuning{GraphDepth: 2, GraphDecay: 0.3}},
		{"d2,decay0.5", query.Tuning{GraphDepth: 2, GraphDecay: 0.5}},
		{"d2,decay0.7", query.Tuning{GraphDepth: 2, GraphDecay: 0.7}},
	}
	legend := "RBO/JACCARD/CHANGED vs shipped (depth 1, default decay). GRAPH-ONLY@8 = mean\n" +
		"top-8 members no other leg surfaced (the KEY metric; 0.00 at depth 1 by\n" +
		"construction, non-zero only if a deeper hop reaches a note direct neighbours\n" +
		"do not). DISPLACED = mean shipped top-8 members pushed out. depth-1 rows vary\n" +
		"decay as a control: decay is inert at one hop (only term is decay^0 = 1)."
	out := runSweep(t, fx, "spreading-activation depth x decay sweep", legend, arms)
	t.Log("\n" + out)
	if err := os.WriteFile(filepath.Join("testdata", "spreading_activation_sweep.txt"), []byte(out), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

// TestSpreadingActivationDepthWeight sweeps hop depth {1,2} x graph fusion weight
// {0.5,1.0,2.0,3.0} at the default decay, the experiment the depth x decay null
// result pointed to. It separates the two coupled levers: depth changes which
// candidates the leg proposes, weight decides whether they survive fusion. The
// depth-1 rows are the control -- raising weight at one hop cannot inject
// GRAPH-ONLY@8 chunks (there are no new candidates to promote), only reorder --
// so a depth-2 GRAPH-ONLY@8 that climbs with weight, against a flat depth-1 row,
// is the coupling made visible, and DISPLACED is what that recall costs.
func TestSpreadingActivationDepthWeight(t *testing.T) {
	fx := setupSpreadingActivation(t)
	arms := []saArm{
		{"SHIPPED(d1,w0.5)", query.Tuning{}},
		{"d1,w1.0", query.Tuning{GraphDepth: 1, GraphWeight: 1.0}},
		{"d1,w2.0", query.Tuning{GraphDepth: 1, GraphWeight: 2.0}},
		{"d1,w3.0", query.Tuning{GraphDepth: 1, GraphWeight: 3.0}},
		{"d2,w0.5", query.Tuning{GraphDepth: 2, GraphWeight: 0.5}},
		{"d2,w1.0", query.Tuning{GraphDepth: 2, GraphWeight: 1.0}},
		{"d2,w2.0", query.Tuning{GraphDepth: 2, GraphWeight: 2.0}},
		{"d2,w3.0", query.Tuning{GraphDepth: 2, GraphWeight: 3.0}},
	}
	legend := "RBO/JACCARD/CHANGED vs shipped (depth 1, weight 0.5). GRAPH-ONLY@8 = mean top-8\n" +
		"members no other leg surfaced -- the KEY metric. Decay fixed at the default 0.5\n" +
		"to isolate depth x weight. depth-1 rows are the control: raising weight at one\n" +
		"hop promotes no NEW candidates, so GRAPH-ONLY@8 stays flat; a depth-2 row that\n" +
		"climbs with weight is the coupling. DISPLACED = shipped top-8 members pushed out,\n" +
		"the relevance cost of the higher weight."
	out := runSweep(t, fx, "spreading-activation depth x graph-weight sweep", legend, arms)
	t.Log("\n" + out)
	if err := os.WriteFile(filepath.Join("testdata", "spreading_activation_weight_sweep.txt"), []byte(out), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}
