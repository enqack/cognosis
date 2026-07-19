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

// Should the keyword leg fall back to OR when AND returns too little, and at
// what threshold?
//
// This exists because TestKeywordANDvsOR cannot answer it. That experiment
// compares the two connectives on the head-drawn query set, where both arms
// saturate a pool of 50 and report EMPTY-Q 0/30 -- a corpus on which AND never
// starves, so the failure a fallback would fix does not occur and "OR is not
// better" is a statement about the fixture. Same shape as the ceiling run that
// reported no headroom because the keyword leg could not be wrong.
//
// The motivating failure is from the real vault, not this corpus: a four-term
// query built from strings unique to one note returned a single FTS candidate --
// belonging to a different note -- and the target was absent from the fused
// top-6 entirely. Under OR the same query returned 33 candidates with the
// target at ranks 1, 2 and 7. The terms were distributed across different H2
// sections of the target, and chunking is per-section, so no chunk held all
// four and the conjunction matched none of them.
//
// Two things this deliberately does NOT do. It asserts no quality threshold --
// it reports a sweep and fails only on broken premises. And it changes no
// production code: the fallback is simulated by selecting between two probes,
// so the numbers exist before the design is committed to.
func TestKeywordORFallbackSweep(t *testing.T) {
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

	// Thresholds. 0 is shipped AND and 51 (> pool) is OR-always, so the sweep
	// brackets both endpoints rather than reasoning about them.
	//
	// 1 and 2 are the decision-relevant pair and neither can be interpolated:
	// the real-vault query returned exactly one AND candidate, so a
	// fire-only-on-empty rule would NOT have rescued it and a fire-below-two
	// rule would. The rest locate where collateral damage on healthy queries
	// starts.
	ns := []int{0, 1, 2, 3, 5, 8, 16, 32, 51}

	if len(c.StarveQueries) == 0 {
		t.Fatal("corpus generated no starving queries; spec.StarveQueries/DistinctivePerChunk are zero")
	}

	// One probe shape for both sets. Starving queries carry a target note, so
	// their headline metric is target-note recall; healthy queries carry only a
	// cluster and fall back to cluster relevance.
	// Split by regime. A query whose AND leg returns one wrong candidate and one
	// whose AND leg returns nothing are different experiments: only the former
	// can distinguish N=1 from N=2, and averaging them together hides the very
	// thing the threshold sweep exists to measure.
	var totalProbes, partialProbes []fallbackProbe
	for _, q := range c.StarveQueries {
		p := fallbackProbe{text: q.Text, cluster: q.Cluster, target: q.NoteID, hasTarget: true}
		if q.HasDecoy {
			partialProbes = append(partialProbes, p)
			continue
		}
		totalProbes = append(totalProbes, p)
	}
	if len(partialProbes) == 0 {
		t.Fatal("no partially-starving queries generated; spec.StarvePartialFrac is zero")
	}
	healthyProbes := make([]fallbackProbe, 0, len(c.Queries))
	for _, q := range c.Queries {
		healthyProbes = append(healthyProbes, fallbackProbe{text: q.Text, cluster: q.Cluster})
	}

	total := measureFallback(ctx, t, c, totalProbes, filter, ns, pool, topK, rrfK, graphWeight)
	partial := measureFallback(ctx, t, c, partialProbes, filter, ns, pool, topK, rrfK, graphWeight)
	healthy := measureFallback(ctx, t, c, healthyProbes, filter, ns, pool, topK, rrfK, graphWeight)

	var b strings.Builder
	fmt.Fprintf(&b, "keyword leg: OR fallback threshold sweep -- %d chunks, pool=%d, top-%d\n",
		spec.Notes*spec.ChunksPerNote, pool, topK)
	fmt.Fprintf(&b, "%d starving queries: %d markers, one from each of %d chunks of ONE target note\n",
		len(c.StarveQueries), len(c.StarveQueries[0].Sections), len(c.StarveQueries[0].Sections))
	fmt.Fprintf(&b, "%d healthy queries: %d frequent-head terms from one cluster\n",
		len(c.Queries), queryTerms)
	fmt.Fprintf(&b, "marker pool %d over %d chunks (~%.1f chunks share a marker)\n\n",
		distinctiveVocabSize(spec), spec.Notes*spec.ChunksPerNote,
		float64(spec.Notes*spec.ChunksPerNote*spec.DistinctivePerChunk)/float64(distinctiveVocabSize(spec)))
	b.WriteString("An arm is `use OR when AND returned fewer than N candidates`. N=0 never\n" +
		"falls back and is the shipped leg; N>pool is OR-always. FIRED counts the\n" +
		"queries on which the arm actually substituted the OR leg.\n\n")

	b.WriteString("Ground truth for both starving sets is the TARGET NOTE, not the cluster.\n" +
		"TGT-IN-CAND is the keyword leg reaching the note at all; TGT-RECALL is it\n" +
		"surviving into the fused top-8. The gap between them is fusion burying a\n" +
		"candidate the leg did find -- a different defect from never finding it.\n\n")

	fmt.Fprintf(&b, "TOTALLY STARVING (%d queries) -- AND returns nothing at all.\n", total.queries)
	b.WriteString("Every N >= 1 fires here, so this set answers 'does falling back help',\n")
	b.WriteString("not 'which N'.\n")
	writeFallbackTable(&b, ns, total, total.queries, true)

	fmt.Fprintf(&b, "\nPARTIALLY STARVING (%d queries) -- AND returns exactly one candidate,\n", partial.queries)
	b.WriteString("belonging to a DIFFERENT note. This is the regime the real vault was in,\n")
	b.WriteString("and the only one where the threshold is a real choice: N=1 cannot fire\n")
	b.WriteString("(one candidate is not fewer than one), N=2 can.\n")
	writeFallbackTable(&b, ns, partial, partial.queries, true)

	b.WriteString("\nHEALTHY SET -- collateral damage on queries that already work.\n")
	b.WriteString("JACCARD is the fused top-8 against the N=0 baseline; 1.000 means the arm\n")
	b.WriteString("never fired here. Read it together with the histogram below, not alone.\n")
	writeFallbackTable(&b, ns, healthy, len(c.Queries), false)

	fmt.Fprintf(&b, "\nAND candidate-count histogram, healthy set: %s\n", healthy.histogram())
	b.WriteString("A saturated histogram means every N <= pool fires on zero healthy queries,\n" +
		"so this table understates collateral damage for a smaller vault. The real\n" +
		"vault that motivated this holds ~103 chunks and does not saturate.\n")
	fmt.Fprintf(&b, "AND candidate-count histogram, partially-starving set: %s\n", partial.histogram())
	fmt.Fprintf(&b, "\npositive control: AND returned zero candidates on %d of %d totally-starving queries\n",
		total.byN[0].emptyQueries, total.queries)
	fmt.Fprintf(&b, "positive control: on the partial set N=1 fired %d times, N=2 fired %d times\n",
		partial.byN[1].fired, partial.byN[2].fired)
	writeArtifact(t, "tsquery_or_fallback_sweep.txt", b.String())
	t.Log("\n" + b.String())

	// Premise 1: the starving set must actually starve. If AND found candidates
	// for every one of them, the cross-chunk markers failed to produce a conjunction
	// the corpus cannot satisfy, every arm collapses onto the same leg, and the
	// whole sweep measures one thing nine times. The fixture is the suspect
	// here, not the connective.
	if total.byN[0].emptyQueries == 0 {
		t.Fatalf("AND returned candidates for all %d totally-starving queries: markers are "+
			"not unique enough per chunk, so every arm below measures the same leg",
			total.queries)
	}

	// Premise 2: the partial set must actually be partial. Its whole purpose is
	// to sit in the one-candidate regime; if the decoys did not land, AND
	// returns zero there too and the set is a duplicate of the total one.
	if partial.byN[0].emptyQueries == partial.queries {
		t.Fatalf("AND returned nothing for all %d supposedly-partial queries: the decoy "+
			"chunks were not planted, so no arm can separate N=1 from N=2", partial.queries)
	}

	// Premise 3: the threshold must discriminate. On the partial set N=1 cannot
	// fire (one candidate is not fewer than one) while N=2 must, which is the
	// entire reason that set exists. If they agree, either the decoys are wrong
	// or fallbackRows is.
	if partial.byN[1].fired == partial.byN[2].fired {
		t.Fatalf("N=1 and N=2 both fired on %d partially-starving queries: the threshold "+
			"does not select in the regime built to make it selectable",
			partial.byN[1].fired)
	}
}

// TestFallbackRowsSelects pins the selection rule itself, with no corpus and no
// database. The sweep above cannot check it: its starving set is uniformly
// empty, so every threshold behaves identically there and a broken comparison
// would be invisible.
func TestFallbackRowsSelects(t *testing.T) {
	and := make([]store.RankedChunk, 3)
	or := make([]store.RankedChunk, 20)

	for _, tc := range []struct {
		name  string
		and   []store.RankedChunk
		n     int
		useOR bool
	}{
		{"n=0 never falls back", and, 0, false},
		{"n=0 never falls back even when empty", nil, 0, false},
		{"empty AND falls back at n=1", nil, 1, true},
		{"non-empty AND holds at n=1", and, 1, false},
		{"3 candidates hold at n=3", and, 3, false},
		{"3 candidates fall back at n=4", and, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, useOR := fallbackRows(tc.and, or, tc.n)
			if useOR != tc.useOR {
				t.Fatalf("useOR = %v, want %v", useOR, tc.useOR)
			}
			want := len(tc.and)
			if tc.useOR {
				want = len(or)
			}
			if len(got) != want {
				t.Fatalf("returned %d rows, want %d", len(got), want)
			}
		})
	}
}

// fallbackRows returns the AND leg, or the OR leg when AND returned fewer than
// n candidates. n=0 never falls back and is the shipped behaviour; n greater
// than the pool is OR-always.
//
// Nothing in the request path does this -- store.RankFTS takes no mode and
// store.TSQueryOr is measurement-only. This is the change under evaluation,
// simulated so it can be measured before it is written.
func fallbackRows(and, or []store.RankedChunk, n int) ([]store.RankedChunk, bool) {
	if len(and) < n {
		return or, true
	}
	return and, false
}

// fallbackProbe is one query in the shape both sets share. hasTarget marks the
// starving set, whose ground truth is a specific note rather than a cluster.
type fallbackProbe struct {
	text      string
	cluster   int
	target    uuid.UUID
	hasTarget bool
}

// fallbackArm accumulates one threshold's results over one query set.
type fallbackArm struct {
	n                 int
	candidates        int
	candidateRelevant int
	emptyQueries      int
	topRelevant       int
	topTotal          int
	fired             int
	jacSum            float64

	// Target-note metrics, populated only for the starving set.
	// targetInCand and targetInTop separate "the keyword leg never surfaced the
	// note" from "the leg surfaced it and fusion buried it" -- different bugs
	// with different fixes, indistinguishable from a single recall number.
	targetInCand int
	targetInTop  int
}

// fallbackRun is one query set measured across every threshold.
type fallbackRun struct {
	byN     map[int]*fallbackArm
	queries int
	// andCounts is the per-query AND candidate count, kept so the artifact can
	// show pool saturation directly. A Jaccard of 1.000 on the healthy set is
	// only reassuring if the histogram shows the arm had a chance to fire.
	andCounts []int
}

func (r *fallbackRun) histogram() string {
	buckets := []struct {
		label  string
		lo, hi int
	}{
		{"0", 0, 0}, {"1-4", 1, 4}, {"5-15", 5, 15},
		{"16-30", 16, 30}, {"31-49", 31, 49}, {"50+", 50, 1 << 30},
	}
	counts := make([]int, len(buckets))
	for _, n := range r.andCounts {
		for i, bk := range buckets {
			if n >= bk.lo && n <= bk.hi {
				counts[i]++
				break
			}
		}
	}
	parts := make([]string, 0, len(buckets))
	for i, bk := range buckets {
		parts = append(parts, fmt.Sprintf("%s:%d", bk.label, counts[i]))
	}
	return strings.Join(parts, "  ")
}

// measureFallback probes each query twice -- once per connective -- and derives
// every threshold from those two slices. Fusion is evaluated once per side, not
// once per threshold, so adding N values to the sweep costs nothing: the arms
// differ only in which of the two cached fusions they select.
func measureFallback(
	ctx context.Context, t *testing.T, c *Corpus, queries []fallbackProbe,
	filter store.Filter, ns []int, pool, topK, rrfK int, graphWeight float64,
) *fallbackRun {
	t.Helper()

	run := &fallbackRun{byN: map[int]*fallbackArm{}, queries: len(queries)}
	for _, n := range ns {
		run.byN[n] = &fallbackArm{n: n}
	}

	// side is one connective's outcome for one query: the leg itself plus the
	// fused top-K it produces once the graph leg has been seeded from it.
	type side struct {
		rows         []store.RankedChunk
		top          []uuid.UUID
		topRelevant  int
		relevant     int
		targetInCand bool
		targetInTop  bool
	}

	for _, q := range queries {
		vec, err := c.Provider.EmbedQuery(ctx, q.text)
		if err != nil {
			t.Fatal(err)
		}
		vp, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		sides := map[bool]*side{}
		for _, m := range []struct {
			useOR bool
			mode  store.TSQueryMode
		}{{false, store.TSQueryWebsearch}, {true, store.TSQueryOr}} {
			fp, err := c.Store.ProbeFTSMode(ctx, q.text, m.mode, filter, pool, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			s := &side{rows: fp.Rows}
			for _, r := range fp.Rows {
				if c.Provider.Labels[r.Content] == q.cluster {
					s.relevant++
				}
				if q.hasTarget && r.NoteID == q.target {
					s.targetInCand = true
				}
			}

			// Seed the graph leg from this side's own candidates, as Run does.
			// Seeds are deliberately not held fixed across sides: a fallback
			// that changes membership also changes what the graph leg is given,
			// and that downstream effect is part of what it costs.
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

			fused := query.FuseRRF(rrfK, func(ch store.RankedChunk) uuid.UUID { return ch.ChunkID },
				[]query.Leg[store.RankedChunk]{
					{Items: vp.Rows, Weight: 1},
					{Items: fp.Rows, Weight: 1},
					{Items: gp.Rows, Weight: graphWeight},
				})
			if len(fused) > topK {
				fused = fused[:topK]
			}
			for _, f := range fused {
				s.top = append(s.top, f.Item.ChunkID)
				if c.Provider.Labels[f.Item.Content] == q.cluster {
					s.topRelevant++
				}
				if q.hasTarget && f.Item.NoteID == q.target {
					s.targetInTop = true
				}
			}
			sides[m.useOR] = s
		}

		run.andCounts = append(run.andCounts, len(sides[false].rows))

		for _, n := range ns {
			_, useOR := fallbackRows(sides[false].rows, sides[true].rows, n)
			s := sides[useOR]
			a := run.byN[n]
			if useOR {
				a.fired++
			}
			a.candidates += len(s.rows)
			a.candidateRelevant += s.relevant
			if len(s.rows) == 0 {
				a.emptyQueries++
			}
			a.topRelevant += s.topRelevant
			a.topTotal += len(s.top)
			a.jacSum += jaccard(sides[false].top, s.top)
			if s.targetInCand {
				a.targetInCand++
			}
			if s.targetInTop {
				a.targetInTop++
			}
		}
	}
	return run
}

// writeFallbackTable renders one query set. withTarget adds the note-level
// columns, which exist only for the starving set.
func writeFallbackTable(b *strings.Builder, ns []int, r *fallbackRun, total int, withTarget bool) {
	if withTarget {
		fmt.Fprintf(b, "%-18s %11s %11s %9s %12s %11s %9s %7s\n",
			"ARM", "CANDIDATES", "CAND-PREC", "EMPTY-Q", "TGT-IN-CAND", "TGT-RECALL", "JACCARD", "FIRED")
	} else {
		fmt.Fprintf(b, "%-18s %11s %11s %9s %10s %9s %7s\n",
			"ARM", "CANDIDATES", "CAND-PREC", "EMPTY-Q", "TOPK-REL", "JACCARD", "FIRED")
	}
	n := float64(r.queries)
	for _, k := range ns {
		a := r.byN[k]
		prec, top := 0.0, 0.0
		if a.candidates > 0 {
			prec = float64(a.candidateRelevant) / float64(a.candidates)
		}
		if a.topTotal > 0 {
			top = float64(a.topRelevant) / float64(a.topTotal)
		}
		if withTarget {
			fmt.Fprintf(b, "%-18s %11.1f %11.3f %7d/%-2d %12.3f %11.3f %9.3f %5d/%-2d\n",
				fallbackArmName(k), float64(a.candidates)/n, prec,
				a.emptyQueries, total,
				float64(a.targetInCand)/n, float64(a.targetInTop)/n,
				a.jacSum/n, a.fired, total)
			continue
		}
		fmt.Fprintf(b, "%-18s %11.1f %11.3f %7d/%-2d %10.3f %9.3f %5d/%-2d\n",
			fallbackArmName(k), float64(a.candidates)/n, prec,
			a.emptyQueries, total, top, a.jacSum/n, a.fired, total)
	}
}

func fallbackArmName(n int) string {
	switch n {
	case 0:
		return "AND (shipped)"
	case 51:
		return "OR-always"
	default:
		return fmt.Sprintf("fallback@%d", n)
	}
}
