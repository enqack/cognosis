package retrievaleval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// Is graphWeight = 0.5 the right fusion weight for the graph leg?
//
// This exists because 0.5 is the last free retrieval parameter with no
// measurement behind it. The vector leg has recall-vs-exact and capacity
// sweeps, the keyword leg has ceiling, precision and fallback sweeps -- the
// graph leg has only the on/off DisableGraph toggle inside the fused-overlap
// test. Its weight has never been varied. On real traffic the leg is emergent
// with vault maturity: a no-op on a young vault (every candidate it surfaced,
// the vector leg already had), a median of ~+40% unique pool expansion on the
// mature one. A leg with that trajectory should not carry an unexamined
// constant into the regime where it starts to matter.
//
// The sweep runs the real engine (Run, not a leg simulation) across a weight
// ladder and reports, against the shipped 0.5 arm: top-K overlap (RBO,
// Jaccard, queries changed) and the attribution trade the weight actually
// controls -- how many graph-only candidates (chunks no other leg surfaced)
// sit in the fused top-K, versus how many shipped-arm members the weight
// change displaced.
//
// Two deliberate non-goals, same as the other sweeps: no quality threshold is
// asserted (the corpus carries cluster labels, not relevance labels, so
// "better" is not measurable here -- only "different"), and the test fails
// only on broken premises.
func TestGraphWeightSweep(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)

	const pool = 50
	filter := store.Filter{}
	opts := query.Options{}

	// The ladder. Zero Tuning is the shipped arm -- TestTuningExplicitConstantsMatchZero
	// pins that spelling to GraphWeight: 0.5 -- so the deployed weight is a
	// measured cell, not an interpolation. The two leftmost arms bracket the
	// leg's absence: DisableGraph removes it, while w=0 (the negative sentinel;
	// zero would mean "unset") keeps its items in the fused set at score 0. If
	// they ever disagree, zero-scored graph items are reaching the top-K.
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
	const shippedArm = "SHIPPED(w=0.5)"

	// Probe pass: reconstruct each leg's membership once per query, mirroring
	// Run's leg assembly (vector; FTS with the shipped @2 OR fallback; graph
	// seeded from the other legs' notes). Chunk content is the join key between
	// probe rows and fused Results -- the Synth corpus makes it unique per
	// chunk, which Provider.Labels already relies on.
	//
	// graphOnly is the set the weight is FOR: chunks only the link graph can
	// surface. A graph candidate the vector leg also found gets a fused score
	// with or without the graph leg; only the graph-only ones live or die by
	// this weight.
	type probeSets struct {
		vector, fts, graph map[string]bool
		graphOnly          map[string]bool
	}
	probes := make([]probeSets, len(c.Queries))
	for i, q := range c.Queries {
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
		// Mirror Run's fallback exactly: below the threshold, re-run with OR
		// and keep the disjunction only when it found more.
		if len(ftsRows) < 2 {
			op, err := c.Store.ProbeFTSMode(ctx, q.Text, store.TSQueryOr, filter, pool, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(op.Rows) > len(ftsRows) {
				ftsRows = op.Rows
			}
		}

		seen := map[uuid.UUID]bool{}
		var seeds []uuid.UUID
		for _, rows := range [][]store.RankedChunk{vp.Rows, ftsRows} {
			for _, r := range rows {
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

		ps := probeSets{
			vector:    contentSet(vp.Rows),
			fts:       contentSet(ftsRows),
			graph:     contentSet(gp.Rows),
			graphOnly: map[string]bool{},
		}
		for content := range ps.graph {
			if !ps.vector[content] && !ps.fts[content] {
				ps.graphOnly[content] = true
			}
		}
		probes[i] = ps
	}

	// Shipped pass, with stats. The stats double as the premise check that the
	// probe reconstruction above measured the same engine that fused: if the
	// per-leg counts diverge, the graph-only classification is fiction.
	shipped := make([][]query.Result, len(c.Queries))
	graphContributed := 0
	for i, q := range c.Queries {
		c.Engine.Tuning = query.Tuning{}
		res, stats, err := c.Engine.RunWithStats(ctx, q.Text, opts)
		if err != nil {
			t.Fatal(err)
		}
		shipped[i] = res
		if stats.Graph > 0 {
			graphContributed++
		}
		if stats.Vector != len(probes[i].vector) || stats.FTS != len(probes[i].fts) || stats.Graph != len(probes[i].graph) {
			t.Fatalf("query %d: probe reconstruction diverged from the engine "+
				"(probe vector=%d fts=%d graph=%d, engine vector=%d fts=%d graph=%d): "+
				"the graph-only classification below would describe a different engine than the one that fused",
				i, len(probes[i].vector), len(probes[i].fts), len(probes[i].graph),
				stats.Vector, stats.FTS, stats.Graph)
		}
	}

	// Premise: the graph leg must have candidates to weigh. A corpus whose link
	// graph reaches nothing makes every arm identical and the sweep measures
	// one configuration six times -- the fixture is the suspect, not the weight.
	if graphContributed == 0 {
		t.Fatalf("graph leg contributed 0 candidates on all %d queries: spec.LinkDegree "+
			"produced no reachable links, so no weight can differ from any other", len(c.Queries))
	}
	graphOnlyTotal := 0
	for i := range probes {
		graphOnlyTotal += len(probes[i].graphOnly)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "graph-leg fusion weight sweep -- %d chunks, %d queries, shipped scan settings\n",
		spec.Notes*spec.ChunksPerNote, len(c.Queries))
	fmt.Fprintf(&b, "shipped weight %.2f; ladder run through the real engine (Tuning.GraphWeight)\n\n", 0.5)
	b.WriteString("DisableGraph removes the leg; w=0 keeps it at score 0 (its items still\n" +
		"enter the fused set -- the negative-sentinel arm exists to measure that\n" +
		"distinction rather than assume it away).\n\n")
	b.WriteString("RBO/JACCARD/CHANGED compare each arm's fused top-8 against the shipped\n" +
		"arm. GRAPH-ONLY@8 is the per-query mean of top-8 members no other leg\n" +
		"surfaced -- the candidates this weight exists to admit. DISPLACED is the\n" +
		"per-query mean of shipped-arm top-8 members the arm pushed out; the two\n" +
		"columns together are the trade the weight controls.\n\n")
	fmt.Fprintf(&b, "graph leg contributed candidates on %d/%d queries; %d graph-only candidates total\n\n",
		graphContributed, len(c.Queries), graphOnlyTotal)
	fmt.Fprintf(&b, "%-16s %8s %8s %9s %13s %10s\n",
		"ARM", "RBO", "JACCARD", "CHANGED", "GRAPH-ONLY@8", "DISPLACED")

	// w=0 vs DisableGraph: identical everywhere would mean no zero-scored graph
	// item ever reached the top-8 on this corpus. Reported, not asserted --
	// both outcomes are informative.
	var w0Top [][]string

	for _, arm := range arms {
		c.Engine.Tuning = arm.tuning

		var sumRBO, sumJac, sumGraphOnly, sumDisplaced float64
		changed := 0
		for i, q := range c.Queries {
			res := shipped[i]
			if arm.name != shippedArm {
				var err error
				res, err = c.Engine.Run(ctx, q.Text, opts)
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
			if arm.tuning.GraphWeight < 0 {
				contents := make([]string, 0, len(top))
				for _, r := range top {
					contents = append(contents, r.Content)
				}
				w0Top = append(w0Top, contents)
			}
		}
		n := float64(len(c.Queries))
		fmt.Fprintf(&b, "%-16s %8.3f %8.3f %7d/%-2d %13.2f %10.2f\n",
			arm.name, sumRBO/n, sumJac/n, changed, len(c.Queries),
			sumGraphOnly/n, sumDisplaced/n)
	}
	c.Engine.Tuning = query.Tuning{}

	// The w=0 vs DisableGraph comparison needs the DisableGraph top-8s, which
	// were computed in the loop above only as overlap-vs-shipped. Re-run the
	// one arm rather than complicating the loop: 30 queries is cheap.
	c.Engine.Tuning = query.Tuning{DisableGraph: true}
	w0DiffersFromOff := 0
	for i, q := range c.Queries {
		res, err := c.Engine.Run(ctx, q.Text, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) > query.DefaultTopK {
			res = res[:query.DefaultTopK]
		}
		if len(res) != len(w0Top[i]) {
			w0DiffersFromOff++
			continue
		}
		for j, r := range res {
			if r.Content != w0Top[i][j] {
				w0DiffersFromOff++
				break
			}
		}
	}
	c.Engine.Tuning = query.Tuning{}
	fmt.Fprintf(&b, "\nw=0 vs DisableGraph: top-8 differs on %d/%d queries\n", w0DiffersFromOff, len(c.Queries))
	b.WriteString("(0 means no zero-scored graph item reached the top-8 here -- the two\n" +
		"spellings of 'no graph influence' coincide on this corpus; nonzero means\n" +
		"score-0 insertions are occupying slots, the regime the struct comment on\n" +
		"Tuning.DisableGraph warns about.)\n")

	writeArtifact(t, "graph_weight_sweep.txt", b.String())
	t.Log("\n" + b.String())
}

func contentSet(rows []store.RankedChunk) map[string]bool {
	s := make(map[string]bool, len(rows))
	for _, r := range rows {
		s[r.Content] = true
	}
	return s
}
