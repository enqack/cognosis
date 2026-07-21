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

// Does deciding keyword MEMBERSHIP at the note level recover the cross-heading
// target that per-chunk AND loses, without paying OR's collateral?
//
// This is the experiment the OR-fallback sweep sets up but cannot finish. That
// sweep established the disease (per-chunk AND starves when a note's terms span
// its H2 sections) and priced the OR fallback as the treatment. The fallback
// works by widening the connective, so it admits every note in the corpus that
// carries a single incidental query term -- the precision cost the sweep reports
// as roughly half the fused top-8 on healthy queries.
//
// Note-level membership is the alternative treatment: keep AND semantics, but
// test them against the stored `notes.fts` tsvector (to_tsvector over the whole
// note body) rather than any single chunk. A scattered target AND-matches at the
// note level and is recovered; a note that merely shares one term does not, so
// the OR blast radius never opens. This is the shipped fallback (RankFTSNoteLevel
// on the request path); the sweep prices it through the measurement probe
// (store.ProbeFTSNoteLevel), which runs the same SQL.
//
// The five arms, all fused identically (vector + this keyword set + graph seeded
// from it, shipped rrfK/graphWeight, top-8):
//   - AND        per-chunk websearch (the primary leg)
//   - OR         per-chunk disjunction (OR-always)
//   - FALLBACK@2 the old fallback: OR when AND returned fewer than 2
//   - NOTE-LEVEL note-level membership as an always-on leg
//   - NL-FB@2    the shipped chain: note-level only when AND starves (AND < 2)
//
// Like the sibling sweeps this asserts broken premises only -- no quality
// threshold.
func TestNoteLevelMembershipSweep(t *testing.T) {
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

	if len(c.StarveQueries) == 0 {
		t.Fatal("corpus generated no starving queries; spec.StarveQueries/DistinctivePerChunk are zero")
	}

	// Split the starving set the same way the fallback sweep does: a totally
	// starving query returns nothing from AND, a partial one returns exactly one
	// candidate belonging to a DIFFERENT note (the real-vault regime). They are
	// different experiments and averaging them hides the partial set's precision
	// story.
	var totalProbes, partialProbes []membershipProbe
	for _, q := range c.StarveQueries {
		p := membershipProbe{text: q.Text, cluster: q.Cluster, target: q.NoteID, hasTarget: true}
		if q.HasDecoy {
			partialProbes = append(partialProbes, p)
			continue
		}
		totalProbes = append(totalProbes, p)
	}
	if len(partialProbes) == 0 {
		t.Fatal("no partially-starving queries generated; spec.StarvePartialFrac is zero")
	}
	healthyProbes := make([]membershipProbe, 0, len(c.Queries))
	for _, q := range c.Queries {
		healthyProbes = append(healthyProbes, membershipProbe{text: q.Text, cluster: q.Cluster})
	}

	total := measureMembership(ctx, t, c, totalProbes, filter, pool, topK, rrfK, graphWeight)
	partial := measureMembership(ctx, t, c, partialProbes, filter, pool, topK, rrfK, graphWeight)
	healthy := measureMembership(ctx, t, c, healthyProbes, filter, pool, topK, rrfK, graphWeight)

	var b strings.Builder
	fmt.Fprintf(&b, "keyword membership: per-chunk AND/OR/fallback vs note-level -- %d chunks, pool=%d, top-%d\n",
		spec.Notes*spec.ChunksPerNote, pool, topK)
	fmt.Fprintf(&b, "%d totally-starving, %d partially-starving, %d healthy queries; rrfK=%d, graph weight %.2f\n\n",
		total.queries, partial.queries, len(c.Queries), rrfK, graphWeight)
	b.WriteString("Every arm fuses vector + its keyword set + graph(seeded from that set).\n" +
		"TGT-IN-CAND is the keyword leg reaching the target note at all; TGT-RECALL is\n" +
		"it surviving fusion into the top-8. CAND is the mean keyword candidate count;\n" +
		"CAND-PREC is the fraction of them in the query's own cluster (precision proxy).\n\n")

	fmt.Fprintf(&b, "TOTALLY STARVING (%d) -- AND finds nothing; can note-level reach the note?\n", total.queries)
	writeMembershipTable(&b, total, true)
	fmt.Fprintf(&b, "\nPARTIALLY STARVING (%d) -- AND returns one WRONG note (the real-vault regime).\n", partial.queries)
	writeMembershipTable(&b, partial, true)
	b.WriteString("\nHEALTHY -- collateral: JACCARD is fused top-8 vs the AND baseline (1.000 = identical).\n")
	writeMembershipTable(&b, healthy, false)

	writeArtifact(t, "note_level_membership.txt", b.String())
	t.Log("\n" + b.String())

	// Premise 1: the totally-starving set must starve the shipped leg, or there
	// is no defect to fix and every arm measures the same thing.
	if total.arm("AND").targetInCand > 0 {
		t.Fatalf("AND reached the target note on %d/%d totally-starving queries: the corpus "+
			"does not starve per-chunk AND, so note-level membership has nothing to recover",
			total.arm("AND").targetInCand, total.queries)
	}
	// Premise 2: note-level membership must be a real intervention -- it has to
	// reach at least one target the shipped leg missed, else the probe SQL is
	// inert and the comparison is vacuous.
	if total.arm("NOTE-LEVEL").targetInCand == 0 {
		t.Fatalf("note-level membership reached the target on 0/%d totally-starving queries: "+
			"the aggregate-tsvector membership test is not firing", total.queries)
	}
}

type membershipProbe struct {
	text      string
	cluster   int
	target    uuid.UUID
	hasTarget bool
}

type membershipArm struct {
	name         string
	candidates   int
	candRelevant int
	targetInCand int // queries whose keyword leg reached the target note (0..n)
	targetInTop  int // queries whose fused top-8 held the target note (0..n)
	targetChunks int // target-note chunks occupying the top-8, summed (a concentration signal, not recall)
	topRelevant  int
	topTotal     int
	jacSum       float64
}

type membershipRun struct {
	arms    []*membershipArm
	queries int
}

func (r *membershipRun) arm(name string) *membershipArm {
	for _, a := range r.arms {
		if a.name == name {
			return a
		}
	}
	panic("no arm " + name)
}

// measureMembership probes each query's keyword leg four ways, fuses each with
// the shared vector leg and a graph leg seeded from that keyword set, and
// accumulates per-arm target-note and precision metrics. The AND arm is the
// jaccard baseline every other arm is compared against.
func measureMembership(
	ctx context.Context, t *testing.T, c *Corpus, queries []membershipProbe,
	filter store.Filter, pool, topK, rrfK int, graphWeight float64,
) *membershipRun {
	t.Helper()

	run := &membershipRun{queries: len(queries)}
	for _, name := range []string{"AND", "OR", "FALLBACK@2", "NOTE-LEVEL", "NL-FB@2"} {
		run.arms = append(run.arms, &membershipArm{name: name})
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

		andP, err := c.Store.ProbeFTSMode(ctx, q.text, store.TSQueryWebsearch, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		orP, err := c.Store.ProbeFTSMode(ctx, q.text, store.TSQueryOr, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		nlP, err := c.Store.ProbeFTSNoteLevel(ctx, q.text, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		// FALLBACK@2 mirrors the shipped rule: OR only when AND < 2 AND OR found
		// strictly more. Otherwise the AND leg stands.
		fallback := andP.Rows
		if len(andP.Rows) < 2 && len(orP.Rows) > len(andP.Rows) {
			fallback = orP.Rows
		}
		// NL-FB@2 is the candidate design: identical gate, note-level membership
		// as the fallback instead of OR. It leaves healthy queries on pure AND
		// (the gate never fires when AND saturates) and swaps in the precise
		// note-level set only when AND starves.
		nlFallback := andP.Rows
		if len(andP.Rows) < 2 && len(nlP.Rows) > len(andP.Rows) {
			nlFallback = nlP.Rows
		}

		var andTop []uuid.UUID
		for i, kw := range []struct {
			name string
			rows []store.RankedChunk
		}{
			{"AND", andP.Rows}, {"OR", orP.Rows},
			{"FALLBACK@2", fallback}, {"NOTE-LEVEL", nlP.Rows},
			{"NL-FB@2", nlFallback},
		} {
			a := run.arm(kw.name)
			a.candidates += len(kw.rows)
			for _, r := range kw.rows {
				if c.Provider.Labels[r.Content] == q.cluster {
					a.candRelevant++
				}
				if q.hasTarget && r.NoteID == q.target {
					a.targetInCand++
					break
				}
			}

			// Seed the graph leg from this arm's own keyword set (plus the shared
			// vector set), as Run does: a membership change also changes what the
			// graph leg is handed, and that is part of the arm's effect.
			seen := map[uuid.UUID]bool{}
			var seeds []uuid.UUID
			for _, rows := range [][]store.RankedChunk{vp.Rows, kw.rows} {
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
					{Items: kw.rows, Weight: 1},
					{Items: gp.Rows, Weight: graphWeight},
				})
			if len(fused) > topK {
				fused = fused[:topK]
			}
			var top []uuid.UUID
			targetHit := false
			for _, f := range fused {
				top = append(top, f.Item.ChunkID)
				if c.Provider.Labels[f.Item.Content] == q.cluster {
					a.topRelevant++
				}
				if q.hasTarget && f.Item.NoteID == q.target {
					targetHit = true // per-query recall: counted once below
					a.targetChunks++ // per-chunk concentration
				}
			}
			if targetHit {
				a.targetInTop++
			}
			a.topTotal += len(top)
			if i == 0 {
				andTop = top
			}
			a.jacSum += jaccard(andTop, top)
		}
	}
	return run
}

func writeMembershipTable(b *strings.Builder, r *membershipRun, withTarget bool) {
	if withTarget {
		fmt.Fprintf(b, "%-12s %8s %9s %12s %11s %9s %8s\n",
			"ARM", "CAND", "CAND-PREC", "TGT-IN-CAND", "TGT-RECALL", "TGT-CHUNK", "JACCARD")
	} else {
		fmt.Fprintf(b, "%-12s %8s %9s %10s %8s\n",
			"ARM", "CAND", "CAND-PREC", "TOPK-REL", "JACCARD")
	}
	n := float64(r.queries)
	for _, a := range r.arms {
		prec, top := 0.0, 0.0
		if a.candidates > 0 {
			prec = float64(a.candRelevant) / float64(a.candidates)
		}
		if a.topTotal > 0 {
			top = float64(a.topRelevant) / float64(a.topTotal)
		}
		if withTarget {
			// TGT-RECALL is a 0..1 per-query hit rate; TGT-CHUNK is the mean count
			// of the target note's chunks that occupied the top-8 (concentration,
			// which can exceed 1 and is not recall).
			fmt.Fprintf(b, "%-12s %8.1f %9.3f %12.3f %11.3f %9.2f %8.3f\n",
				a.name, float64(a.candidates)/n, prec,
				float64(a.targetInCand)/n, float64(a.targetInTop)/n,
				float64(a.targetChunks)/n, a.jacSum/n)
			continue
		}
		fmt.Fprintf(b, "%-12s %8.1f %9.3f %10.3f %8.3f\n",
			a.name, float64(a.candidates)/n, prec, top, a.jacSum/n)
	}
}
