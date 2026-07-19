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

// keywordArms is one corpus measured three ways: shipped ts_rank_cd ordering,
// a ground-truth oracle ordering (the ceiling for any keyword ranker), and the
// keyword leg deleted (the floor).
type keywordArms struct {
	precision                   float64 // share of FTS candidates that are relevant
	reordered, slots            int     // positive control: did the oracle do anything
	oracleChanged, noFTSChanged int
	oracleJac, noFTSJac         float64
}

// measureKeywordArms fuses exactly the way Run fuses -- same k, same weights,
// same ChunkID key -- and holds the graph seeds fixed across arms so the only
// variable is the keyword leg's ordering or presence.
func measureKeywordArms(t *testing.T, ctx context.Context, c *Corpus, rrfK, topK, pool int) keywordArms {
	t.Helper()
	const graphWeight = 0.5
	filter := store.Filter{}
	var a keywordArms

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

		oracleRows := oracleOrder(c, fp.Rows, q.Cluster)
		relevant := 0
		for i := range fp.Rows {
			if c.Provider.Labels[fp.Rows[i].Content] == q.Cluster {
				relevant++
			}
			if fp.Rows[i].ChunkID != oracleRows[i].ChunkID {
				a.reordered++
			}
		}
		a.slots += len(fp.Rows)
		a.precision += float64(relevant)

		vecLeg := query.Leg[store.RankedChunk]{Items: vp.Rows, Weight: 1}
		graphLeg := query.Leg[store.RankedChunk]{Items: gp.Rows, Weight: graphWeight}
		base := fuseTop(rrfK, topK, vecLeg,
			query.Leg[store.RankedChunk]{Items: fp.Rows, Weight: 1}, graphLeg)
		oracle := fuseTop(rrfK, topK, vecLeg,
			query.Leg[store.RankedChunk]{Items: oracleRows, Weight: 1}, graphLeg)
		noFTS := fuseTop(rrfK, topK, vecLeg, graphLeg)

		if j := jaccard(base, oracle); j < 1 {
			a.oracleChanged++
			a.oracleJac += j
		} else {
			a.oracleJac++
		}
		if j := jaccard(base, noFTS); j < 1 {
			a.noFTSChanged++
			a.noFTSJac += j
		} else {
			a.noFTSJac++
		}
	}
	if a.slots > 0 {
		a.precision /= float64(a.slots)
	}
	n := float64(len(c.Queries))
	a.oracleJac /= n
	a.noFTSJac /= n
	return a
}

// TestKeywordHeadroomVsPrecision sweeps the axis the single-point ceiling
// measurement could not settle.
//
// At the default corpus the keyword leg runs at ~99% precision and a perfect
// re-ranking changes 2 of 30 queries -- which reads as "BM25 cannot help", but
// only establishes that it cannot help *when the keyword leg is already almost
// never wrong*. A real vault is not that corpus, and an RRF-damping explanation
// for the flatness was already tried and refuted: sweeping k from 1 to 60 moved
// nothing, so the constraint is precision, not fusion.
//
// BorrowedTerms sets how many of a neighbouring cluster's head words each
// vocabulary carries, which is exactly the channel by which a query retrieves a
// textually plausible but semantically wrong chunk. Sweeping it walks keyword
// precision down and asks how headroom responds.
//
// The shape of the answer is what matters, not any single cell: if headroom
// stays flat as precision falls, the keyword ranker is structurally irrelevant
// under RRF and BM25 is not worth an extension at any precision. If it climbs,
// BM25's value depends on the real vault's keyword precision, and *that*
// becomes the thing to measure before spending anything.
func TestKeywordHeadroomVsPrecision(t *testing.T) {
	requireEval(t)
	ctx := context.Background()

	const (
		pool = 50
		topK = 8
		rrfK = 60
	)

	var b strings.Builder
	b.WriteString("keyword-ranker headroom vs keyword precision\n\n")
	b.WriteString("BorrowedTerms lets each cluster's vocabulary carry its neighbour's head words,\n" +
		"so a conjunction drawn from one cluster can match another's chunks. That is the\n" +
		"only channel producing a wrong-but-plausible keyword match, so it sets precision.\n" +
		"'oracle' orders candidates by ground truth and therefore bounds BM25 from above.\n\n")
	fmt.Fprintf(&b, "%-9s %10s %9s %10s %9s %10s  %s\n",
		"BORROWED", "PRECISION", "ORACLE-J", "ORACLE-CH", "NOFTS-J", "NOFTS-CH", "REORDERED")

	borrowSweep := []int{0, 2, 4, 6, 8}
	headroom := make([]int, 0, len(borrowSweep))
	for _, borrowed := range borrowSweep {
		spec := evalSpec(t)
		spec.BorrowedTerms = borrowed
		c := Build(t, spec)
		a := measureKeywordArms(t, ctx, c, rrfK, topK, pool)
		headroom = append(headroom, a.oracleChanged)
		fmt.Fprintf(&b, "%-9d %10.3f %9.3f %7d/%-2d %9.3f %7d/%-2d  %d/%d\n",
			borrowed, a.precision, a.oracleJac, a.oracleChanged, len(c.Queries),
			a.noFTSJac, a.noFTSChanged, len(c.Queries), a.reordered, a.slots)
	}
	writeArtifact(t, "keyword_precision_sweep.txt", b.String())
	t.Log("\n" + b.String())

	// Positive control for the sweep itself: BorrowedTerms=0 must produce a
	// perfectly precise keyword leg and therefore zero headroom. If even that
	// cell shows headroom, the oracle is measuring something other than
	// relevance and every row is suspect.
	if headroom[0] != 0 {
		t.Errorf("BorrowedTerms=0 gives a keyword leg that cannot be wrong, yet the oracle "+
			"changed %d queries; the oracle is not ordering by relevance", headroom[0])
	}
}
