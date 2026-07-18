package retrievaleval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// Q3: the question Phase 6 turns on. The vector leg under-retrieves at default
// settings (Q1, Q2) — but does that reach the agent? The fused top-8 is what
// actually enters context, and the FTS and graph legs might mask the loss
// entirely. "Overlap is 1.0, no fix needed" is a legitimate and valuable
// outcome, and this test is written so that outcome is reportable rather than
// assumed away.
func TestFusedTopKUnderCorrectedScan(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)

	// The comparison is now pre-fix versus shipped, not default versus
	// corrected: store.Connect applies the fix to every pooled connection, so
	// c.Engine IS the corrected engine. The baseline has to be reconstructed
	// explicitly, or this test would compare the fix against itself and report
	// a reassuring zero.
	//
	// It must go through ConnectWithScanSettings, not the DSN options route:
	// AfterConnect runs after the startup packet, so Connect's settings would
	// overwrite anything the DSN asked for — which is exactly what happened
	// first time round, and the test dutifully reported 0/30 changed.
	baseStore, err := store.ConnectWithScanSettings(ctx, c.DSN, []string{
		"set hnsw.ef_search = 40",
		"set hnsw.iterative_scan = 'off'",
	})
	if err != nil {
		t.Fatalf("connect with pre-fix GUCs: %v", err)
	}
	defer baseStore.Close()

	// Same provider, same table, same tuning — only the scan settings differ.
	baseEngine := &query.Engine{
		Store:     baseStore,
		Providers: []query.ProviderLeg{{Provider: c.Provider, Table: c.Table}},
	}
	assertPoolsDiffer(ctx, t, baseStore, c.Store)

	var b strings.Builder
	fmt.Fprintf(&b, "fused top-8 overlap, pre-fix vs shipped scan settings — %d chunks, %d queries\n",
		spec.Notes*spec.ChunksPerNote, len(c.Queries))
	fmt.Fprintf(&b, "pre-fix = hnsw.ef_search=40 + hnsw.iterative_scan=off (pgvector defaults)\n")
	fmt.Fprintf(&b, "shipped = store.Connect's settings (ef_search=%d + iterative_scan=%s)\n\n",
		store.HNSWEfSearch, store.HNSWIterativeScan)
	fmt.Fprintf(&b, "%-22s %8s %8s %10s %s\n", "CONFIGURATION", "RBO", "JACCARD", "CHANGED", "NOTE")

	type variant struct {
		name   string
		opts   query.Options
		tuning query.Tuning
	}
	variants := []variant{
		{"default scope", query.Options{}, query.Tuning{}},
		{"scope=wide", query.Options{Project: "wide"}, query.Tuning{}},
		{"no graph leg", query.Options{}, query.Tuning{DisableGraph: true}},
		{"vector only", query.Options{}, query.Tuning{DisableGraph: true, CandidatePool: 50}},
	}

	for _, v := range variants {
		baseEngine.Tuning = v.tuning
		c.Engine.Tuning = v.tuning

		var sumRBO, sumJac float64
		changed := 0
		for _, q := range c.Queries {
			base, err := baseEngine.Run(ctx, q.Text, v.opts)
			if err != nil {
				t.Fatal(err)
			}
			fixed, err := c.Engine.Run(ctx, q.Text, v.opts)
			if err != nil {
				t.Fatal(err)
			}
			rbo, jac := Overlap(base, fixed, query.DefaultTopK)
			sumRBO += rbo
			sumJac += jac
			if jac < 1 {
				changed++
			}
		}
		n := float64(len(c.Queries))
		note := "identical — the fix changes nothing the agent sees here"
		if changed > 0 {
			note = fmt.Sprintf("%d/%d queries returned different top-8", changed, len(c.Queries))
		}
		fmt.Fprintf(&b, "%-22s %8.3f %8.3f %10d %s\n",
			v.name, sumRBO/n, sumJac/n, changed, note)
	}
	c.Engine.Tuning = query.Tuning{}

	writeArtifact(t, "fused_overlap.txt", b.String())
	t.Log("\n" + b.String())

	// Deliberately no assertion on the magnitude of the change. Q1 and Q2
	// already establish the leg-level defect with bounds; this test's job is
	// to record whether it propagates, and both answers are informative.
}

// The exact-KNN ground truth is only ground truth if it bypassed the index.
// Record the plan so that claim is checkable rather than asserted.
func TestRecordExactProbePlan(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	c := Build(t, evalSpec(t))

	vec, err := c.Provider.EmbedQuery(ctx, c.Queries[0].Text)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := c.Store.ProbeVectorExact(ctx, c.Table, vec, store.Filter{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if usedHNSW(exact.Plan) {
		t.Fatalf("exact probe used the HNSW index; it is not ground truth:\n%s", exact.Plan)
	}
	body := "EXPLAIN of the exact-KNN ground-truth probe (vectorLegSQL with exact=true).\n" +
		"The `+ 0.0` on the order-by defeats pgvector's index matching. A Seq Scan on the\n" +
		"embeddings relation below is the proof that brute force really is brute force —\n" +
		"without it, \"exact ground truth\" would be an assumption rather than a measurement.\n\n" +
		elideVectors(exact.Plan)
	writeArtifact(t, "explain_vector_exact.txt", body)
	t.Log("\n" + elideVectors(exact.Plan))
}
