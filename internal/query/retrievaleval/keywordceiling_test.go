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

// How much can a better keyword ranker be worth here at all?
//
// BM25 (pg_textsearch, pg_search, vchord_bm25) is a Rust/PGRX extension that
// needs shared_preload_libraries -- a flake change, a Postgres restart, and the
// end of the per-schema test isolation this harness depends on. That is a real
// cost, and it is worth knowing the size of the prize before paying it.
//
// The prize has a measurable ceiling. RRF consumes *rank*, not score, so any
// keyword ranker can only ever permute the FTS candidate list. Replacing
// ts_rank_cd's ordering with a perfect one -- ground-truth relevance, which no
// real ranker can beat -- bounds every possible keyword ranker including BM25.
// If the fused top-8 barely moves under a perfect ranker, BM25 cannot help.
//
// Three arms, each fused exactly the way Run fuses (same k, same weights, same
// ChunkID key, graph seeded from the other legs):
//
//	baseline  the shipped ts_rank_cd ordering
//	oracle    FTS candidates reordered by ground-truth cluster -- the ceiling
//	no-fts    the keyword leg removed entirely -- the floor
//
// baseline-vs-oracle is the headroom above the shipped ranker.
// baseline-vs-no-fts is what the keyword leg contributes at all. The second
// bounds the first: a leg that changes nothing when deleted cannot be improved
// into mattering.
func TestKeywordRankerCeiling(t *testing.T) {
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

	// rrfKSweep tests whether "ordering barely matters" is a fact about this
	// corpus or about the fusion constant. RRF damps rank differences by k: at
	// k=60 the whole 50-deep candidate list spans 1/61..1/110, a 1.8x range,
	// while present-vs-absent spans 0.0164..0. If headroom appears only as k
	// falls, the shipped k is what makes keyword ordering irrelevant -- and no
	// ranker, BM25 included, can change that.
	rrfKSweep := []int{1, 5, 15, 60}
	sweepChanged := map[int]int{}
	sweepJac := map[int]float64{}

	var oracleChanged, noFTSChanged int
	var oracleJac, noFTSJac float64
	// Positive control. If the oracle never actually permutes the candidate
	// list, "oracle == baseline" measures nothing and reports it as a clean
	// negative result -- the exact failure this harness exists to catch.
	reordered, labelled := 0, 0

	for _, q := range c.Queries {
		vec, err := c.Provider.EmbedQuery(ctx, q.Text)
		if err != nil {
			t.Fatal(err)
		}
		vp, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := c.Store.ProbeFTS(ctx, q.Text, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		// Seed the graph leg from the union of the other two, as Run does.
		// Held fixed across all three arms: re-seeding it from each arm's own
		// candidates would let the graph leg move for reasons that have
		// nothing to do with keyword ordering, and the comparison would no
		// longer isolate what it claims to.
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
		gp, err := c.Store.ProbeGraph(ctx, seeds, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		vecLeg := query.Leg[store.RankedChunk]{Items: vp.Rows, Weight: 1}
		graphLeg := query.Leg[store.RankedChunk]{Items: gp.Rows, Weight: graphWeight}

		base := fuseTop(rrfK, topK, vecLeg,
			query.Leg[store.RankedChunk]{Items: fp.Rows, Weight: 1}, graphLeg)
		oracleRows := oracleOrder(c, fp.Rows, q.Cluster)
		for i := range fp.Rows {
			if c.Provider.Labels[fp.Rows[i].Content] == q.Cluster {
				labelled++
			}
			if fp.Rows[i].ChunkID != oracleRows[i].ChunkID {
				reordered++
			}
		}
		oracle := fuseTop(rrfK, topK, vecLeg,
			query.Leg[store.RankedChunk]{Items: oracleRows, Weight: 1}, graphLeg)
		noFTS := fuseTop(rrfK, topK, vecLeg, graphLeg)

		for _, k := range rrfKSweep {
			kb := fuseTop(k, topK, vecLeg,
				query.Leg[store.RankedChunk]{Items: fp.Rows, Weight: 1}, graphLeg)
			ko := fuseTop(k, topK, vecLeg,
				query.Leg[store.RankedChunk]{Items: oracleRows, Weight: 1}, graphLeg)
			j := jaccard(kb, ko)
			sweepJac[k] += j
			if j < 1 {
				sweepChanged[k]++
			}
		}

		if j := jaccard(base, oracle); j < 1 {
			oracleChanged++
			oracleJac += j
		} else {
			oracleJac++
		}
		if j := jaccard(base, noFTS); j < 1 {
			noFTSChanged++
			noFTSJac += j
		} else {
			noFTSJac++
		}
	}

	n := float64(len(c.Queries))
	var b strings.Builder
	fmt.Fprintf(&b, "keyword-ranker headroom -- %d chunks, pool=%d, top-%d, %d queries\n\n",
		spec.Notes*spec.ChunksPerNote, pool, topK, len(c.Queries))
	b.WriteString("RRF consumes rank, so any keyword ranker can only permute the FTS candidate\n" +
		"list. 'oracle' orders it by ground-truth cluster, which no real ranker can beat,\n" +
		"and therefore bounds BM25 from above. 'no-fts' deletes the leg outright.\n\n")
	fmt.Fprintf(&b, "%-28s %8s %10s\n", "ARM (vs shipped ts_rank_cd)", "JACCARD", "CHANGED")
	fmt.Fprintf(&b, "%-28s %8.3f %7d/%d\n", "oracle keyword order", oracleJac/n, oracleChanged, len(c.Queries))
	fmt.Fprintf(&b, "%-28s %8.3f %7d/%d\n", "keyword leg removed", noFTSJac/n, noFTSChanged, len(c.Queries))
	fmt.Fprintf(&b, "\npositive control: %d of %d candidate slots moved under the oracle, %d were labelled relevant\n",
		reordered, len(c.Queries)*pool, labelled)
	b.WriteString("\nheadroom for a perfect keyword ranker, by RRF damping constant\n")
	fmt.Fprintf(&b, "%-12s %8s %10s\n", "RRF k", "JACCARD", "CHANGED")
	for _, k := range rrfKSweep {
		fmt.Fprintf(&b, "%-12d %8.3f %7d/%d\n", k, sweepJac[k]/n, sweepChanged[k], len(c.Queries))
	}
	writeArtifact(t, "keyword_ceiling.txt", b.String())
	t.Log("\n" + b.String())

	// Assert the experiment is answerable, not what the answer is. If deleting
	// the keyword leg changes nothing, the corpus cannot distinguish any two
	// keyword rankers and every number above is vacuous -- that is a defect in
	// the fixture, and it is exactly the state this corpus was just rebuilt to
	// escape.
	// Positive control, asserted rather than merely printed: an oracle that
	// permutes nothing proves nothing, and "0/30 changed" would read as a
	// confident negative result instead of a broken instrument.
	if reordered == 0 {
		t.Fatalf("the oracle never reordered a candidate (%d labelled relevant of %d slots): "+
			"the ceiling arm is inert and its 'no change' result is meaningless",
			labelled, len(c.Queries)*pool)
	}
	if noFTSChanged == 0 {
		t.Errorf("removing the keyword leg changed no query's top-%d: the FTS leg is inert on this "+
			"corpus, so it cannot discriminate ts_rank_cd from BM25 and this measurement is void", topK)
	}
}

// oracleOrder is the best any keyword ranker could do: every truly-relevant
// candidate ahead of every irrelevant one, order otherwise preserved so the
// only variable is relevance. Stable, so the comparison stays deterministic.
func oracleOrder(c *Corpus, rows []store.RankedChunk, cluster int) []store.RankedChunk {
	relevant := make([]store.RankedChunk, 0, len(rows))
	rest := make([]store.RankedChunk, 0, len(rows))
	for _, r := range rows {
		if c.Provider.Labels[r.Content] == cluster {
			relevant = append(relevant, r)
		} else {
			rest = append(rest, r)
		}
	}
	return append(relevant, rest...)
}

// fuseTop fuses legs the way Run does and returns the top-k chunk ids.
// The archived-link penalty is deliberately not applied: it is a post-fusion
// score adjustment driven by note status, identical across all three arms, so
// including it would add noise without changing what is being compared.
// parameter because the arm being compared is "the top-K an agent sees", and
// hardcoding it would quietly couple this experiment to one constant.
//
//nolint:unparam // topK is fixed at the shipped DefaultTopK today; it is a
func fuseTop(k, topK int, legs ...query.Leg[store.RankedChunk]) []uuid.UUID {
	fused := query.FuseRRF(k, func(c store.RankedChunk) uuid.UUID { return c.ChunkID }, legs)
	if len(fused) > topK {
		fused = fused[:topK]
	}
	out := make([]uuid.UUID, len(fused))
	for i, f := range fused {
		out[i] = f.Item.ChunkID
	}
	return out
}

func jaccard(a, b []uuid.UUID) float64 {
	set := make(map[uuid.UUID]bool, len(a))
	for _, id := range a {
		set[id] = true
	}
	inter := 0
	for _, id := range b {
		if set[id] {
			inter++
		}
	}
	union := len(set)
	for _, id := range b {
		if !set[id] {
			union++
		}
	}
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}
