package retrievaleval

import (
	"context"
	"math/rand"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// smallSpec is the smoke-test corpus: big enough to exercise every code path,
// small enough to build in seconds. It is NOT big enough for the planner to
// choose HNSW (Phase 0: seqscan below ~5k chunks), so nothing here asserts
// anything about recall.
func smallSpec() CorpusSpec {
	s := DefaultSpec()
	s.Notes = 120
	s.ChunksPerNote = 3
	s.Clusters = 6
	s.Queries = 6
	return s
}

func TestBuildCorpusIsWellFormed(t *testing.T) {
	ctx := context.Background()
	c := Build(t, smallSpec())

	// Every chunk landed, and every chunk got an embedding.
	total, err := c.Store.CountAllChunks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := smallSpec().Notes * smallSpec().ChunksPerNote
	if total != want {
		t.Errorf("chunks = %d, want %d", total, want)
	}
	embedded, err := c.Store.CountEmbeddings(ctx, c.Table)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != total {
		t.Errorf("embeddings = %d, want %d (one per chunk)", embedded, total)
	}
	missing, err := c.Store.MissingCount(ctx, c.Table)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("%d chunks missing embeddings", missing)
	}
}

// The selective scope must actually be selective AND non-empty. Phase 0 built
// a corpus where every note in the smallest project was also archived, so the
// selective scope held zero live rows and the experiment measured nothing.
func TestSelectiveScopeIsNonEmptyAndSelective(t *testing.T) {
	c := Build(t, smallSpec())
	all, narrow := c.InScope[""], c.InScope["narrow"]
	if narrow == 0 {
		t.Fatal("selective scope 'narrow' holds no live chunks; it cannot exercise a filtered scan")
	}
	if narrow >= all {
		t.Fatalf("scope 'narrow' (%d) is not selective against all (%d)", narrow, all)
	}
	t.Logf("in-scope live chunks: all=%d wide=%d narrow=%d", all, c.InScope["wide"], narrow)
}

// Status variety must exist, or the temporal/status predicates are never
// exercised by any measurement built on this corpus.
func TestCorpusHasStatusVariety(t *testing.T) {
	ctx := context.Background()
	c := Build(t, smallSpec())
	metas, err := c.Store.ListNotes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, m := range metas {
		counts[m.Status]++
	}
	for _, st := range []string{"active", "archived", "falsified"} {
		if counts[st] == 0 {
			t.Errorf("no notes with status %q", st)
		}
		t.Logf("status %-10s %d notes", st, counts[st])
	}
}

// End-to-end: the wired Engine returns results, and the probes agree with it.
func TestCorpusEngineAndProbesAgree(t *testing.T) {
	ctx := context.Background()
	c := Build(t, smallSpec())
	q := c.Queries[0]

	got, err := c.Engine.Run(ctx, q.Text, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("engine returned no results for a query drawn from the corpus vocabulary")
	}

	vec, err := c.Provider.EmbedQuery(ctx, q.Text)
	if err != nil {
		t.Fatal(err)
	}
	approx, err := c.Store.ProbeVector(ctx, c.Table, vec, store.Filter{}, 50, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := c.Store.ProbeVectorExact(ctx, c.Table, vec, store.Filter{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Rows) == 0 {
		t.Fatal("exact probe returned nothing")
	}
	if exact.Plan == "" {
		t.Fatal("exact probe recorded no plan; exactness would be unverifiable")
	}
	// "Brute force is ground truth" holds only if brute force actually
	// bypassed the index. Check, don't assume.
	if strings.Contains(exact.Plan, "hnsw_idx") {
		t.Fatalf("exact probe used the HNSW index; it is not ground truth:\n%s", exact.Plan)
	}

	ag := Agree(approx.Rows, exact.Rows, 50)
	t.Logf("approx=%d/%d exact=%d recall=%.3f ndcg=%.3f kendall=%.3f",
		len(approx.Rows), approx.Requested, len(exact.Rows), ag.Recall, ag.NDCG, ag.Kendall)
	t.Logf("approx used hnsw=%v", strings.Contains(approx.Plan, "hnsw_idx"))

	// ClusterPrecision must be well above chance for a query drawn from a
	// cluster's own vocabulary — otherwise the label/geometry/vocabulary
	// three-way agreement is broken and every later measurement is void.
	cp := ClusterPrecision(c, q, got, len(got))
	chance := 1.0 / float64(smallSpec().Clusters)
	t.Logf("cluster precision = %.3f (chance = %.3f)", cp, chance)
	if cp <= chance {
		t.Errorf("cluster precision %.3f at or below chance %.3f: geometry, label and vocabulary disagree", cp, chance)
	}
}

// TestKeywordLegHasSomethingToRank is the guard the first corpus lacked.
//
// That corpus drew 8 tokens with replacement from a 12-token bag and queried
// with a 5-term conjunction, so the FTS leg returned 0–2 candidates of a
// requested 50 — measured, not estimated, and only noticed after a per-leg
// capacity sweep was added for an unrelated reason. Every keyword number
// recorded before that point was computed over a near-empty candidate set,
// and a BM25-vs-ts_rank_cd comparison on it would have been ranking two items.
//
// The floor is deliberately low. This asserts the leg is *exercised*, not that
// it is good — quality is what the sweeps measure, and pinning a high number
// here would make an ordinary corpus change look like a regression.
func TestKeywordLegHasSomethingToRank(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	c := Build(t, smallSpec())

	const pool = 50
	const floor = 10

	worst := pool + 1
	worstQuery := ""
	for _, q := range c.Queries {
		p, err := c.Store.ProbeFTS(ctx, q.Text, store.Filter{}, pool, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Rows) < worst {
			worst, worstQuery = len(p.Rows), q.Text
		}
	}
	if worst < floor {
		t.Errorf("worst FTS candidate count is %d (query %q), want >= %d; "+
			"websearch_to_tsquery ANDs its terms, so this is the corpus failing to "+
			"satisfy a %d-term conjunction, not the ranker underperforming",
			worst, worstQuery, floor, queryTerms)
	}
}

// Term frequency and document length must actually vary, or BM25's two
// advantages over ts_rank_cd — saturating TF, normalizing by length — are
// unmeasurable on this corpus and any comparison reports a tie by
// construction. Asserts on the generator, so it needs no database.
func TestChunkProseVariesLengthAndTermFrequency(t *testing.T) {
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // reproducibility, not secrecy
	vocab := clusterVocab(0)

	lengths := map[int]bool{}
	maxRepeat := 0
	for i := range 200 {
		text := chunkProse(rng, vocab, i, 0)
		words := strings.Fields(text)
		lengths[len(words)] = true
		counts := map[string]int{}
		for _, w := range words {
			counts[w]++
		}
		for _, v := range vocab {
			maxRepeat = max(maxRepeat, counts[v])
		}
	}
	if len(lengths) < 20 {
		t.Errorf("only %d distinct chunk lengths over 200 chunks; length normalization is a no-op", len(lengths))
	}
	if maxRepeat < 3 {
		t.Errorf("no topic term repeats more than %dx in any chunk; TF saturation is unmeasurable", maxRepeat)
	}
}
