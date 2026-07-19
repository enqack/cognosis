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

// AND versus OR for the keyword leg — the lever the BM25 work pointed at.
//
// Two prior experiments established that the keyword leg contributes
// membership rather than order: deleting it changes 30 of 30 fused top-8
// results, while ordering it perfectly changes ~2 of 30, and that holds across
// both RRF k (1..60) and keyword precision (1.00..0.87). Membership is set by
// the tsquery connective, and websearch_to_tsquery joins terms with AND.
//
// So this measures the choice that matters, and unlike the ranker comparison it
// can measure *quality* rather than churn: changing membership changes what is
// available to retrieve, and the corpus carries ground-truth cluster labels, so
// the top-8's relevance is directly computable.
//
// The tradeoff under test is the classic one. AND is precise and can return
// nothing; OR always finds something and admits chunks matching one incidental
// term. Which wins after RRF fusion with a vector and graph leg is not
// predictable from that description, which is why it is measured.
func TestKeywordANDvsOR(t *testing.T) {
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

	type arm struct {
		mode              store.TSQueryMode
		candidates        int
		candidateRelevant int
		emptyQueries      int
		top8Relevant      int
		top8Total         int
	}
	arms := map[string]*arm{
		"AND (websearch, shipped)": {mode: store.TSQueryWebsearch},
		"OR  (measurement-only)":   {mode: store.TSQueryOr},
	}

	changed := 0
	var jacSum float64
	tops := map[string][]uuid.UUID{}
	// Candidate *sets* per query, not counts. Both modes saturate a pool of 50
	// on this corpus, so equal totals say nothing about whether the connective
	// took effect — the first version of the control asserted on counts and
	// fired a false alarm against a run whose fused output plainly differed.
	candSets := map[string]map[uuid.UUID]bool{}
	candSetDiff := 0

	for _, q := range c.Queries {
		vec, err := c.Provider.EmbedQuery(ctx, q.Text)
		if err != nil {
			t.Fatal(err)
		}
		vp, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		for name, a := range arms {
			fp, err := c.Store.ProbeFTSMode(ctx, q.Text, a.mode, filter, pool, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			a.candidates += len(fp.Rows)
			set := make(map[uuid.UUID]bool, len(fp.Rows))
			for _, r := range fp.Rows {
				set[r.ChunkID] = true
			}
			candSets[name] = set
			if len(fp.Rows) == 0 {
				a.emptyQueries++
			}
			for _, r := range fp.Rows {
				if c.Provider.Labels[r.Content] == q.Cluster {
					a.candidateRelevant++
				}
			}

			// Seed the graph leg from this arm's own candidates, as Run does.
			// Unlike the ranker experiment, seeds must NOT be held fixed here:
			// changing membership legitimately changes what the graph leg is
			// given, and that downstream effect is part of what AND-vs-OR
			// costs or buys.
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
			ids := make([]uuid.UUID, len(fused))
			for i, f := range fused {
				ids[i] = f.Item.ChunkID
				if c.Provider.Labels[f.Item.Content] == q.Cluster {
					a.top8Relevant++
				}
				a.top8Total++
			}
			tops[name] = ids
		}

		for id := range candSets["AND (websearch, shipped)"] {
			if !candSets["OR  (measurement-only)"][id] {
				candSetDiff++
			}
		}

		j := jaccard(tops["AND (websearch, shipped)"], tops["OR  (measurement-only)"])
		jacSum += j
		if j < 1 {
			changed++
		}
	}

	n := float64(len(c.Queries))
	var b strings.Builder
	fmt.Fprintf(&b, "keyword leg: AND vs OR tsquery — %d chunks, pool=%d, top-%d, %d queries\n\n",
		spec.Notes*spec.ChunksPerNote, pool, topK, len(c.Queries))
	b.WriteString("Membership, not ordering, is what the keyword leg contributes to fused output,\n" +
		"and the tsquery connective is what sets membership. Top-8 relevance is the\n" +
		"quality signal: unlike a ranker swap, this changes what is available to retrieve.\n\n")
	fmt.Fprintf(&b, "%-26s %11s %11s %9s %13s\n",
		"ARM", "CANDIDATES", "CAND-PREC", "EMPTY-Q", "TOP8-RELEVANT")
	for _, name := range []string{"AND (websearch, shipped)", "OR  (measurement-only)"} {
		a := arms[name]
		prec, top8 := 0.0, 0.0
		if a.candidates > 0 {
			prec = float64(a.candidateRelevant) / float64(a.candidates)
		}
		if a.top8Total > 0 {
			top8 = float64(a.top8Relevant) / float64(a.top8Total)
		}
		fmt.Fprintf(&b, "%-26s %11.1f %11.3f %7d/%-2d %13.3f\n",
			name, float64(a.candidates)/n, prec, a.emptyQueries, len(c.Queries), top8)
	}
	fmt.Fprintf(&b, "\nfused top-8 differs on %d/%d queries (mean Jaccard %.3f)\n",
		changed, len(c.Queries), jacSum/n)
	fmt.Fprintf(&b, "positive control: %d of %d AND candidates are absent from the OR set\n",
		candSetDiff, arms["AND (websearch, shipped)"].candidates)
	writeArtifact(t, "tsquery_and_vs_or.txt", b.String())
	t.Log("\n" + b.String())

	// Positive control: the two modes must actually produce different candidate
	// sets. If OR returns exactly what AND returns, the connective never took
	// effect — a NULL tsquery, a silently-identical plan — and "no difference"
	// would be a broken probe reported as a finding.
	if candSetDiff == 0 {
		t.Fatalf("every AND candidate also appears in the OR set across all %d queries: the "+
			"connective had no effect and this comparison measured one mode twice",
			len(c.Queries))
	}
}
