package retrievaleval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// Manual: the encoding-specificity (context-prior) sweep against a multi-project
// real-vault dump. It closes P1 with a measurement rather than a structural
// prediction: does a soft same-project boost re-rank anything the graph leg does
// not already? The structural pass found project ~collinear with link community
// (most links within-project), predicting redundancy. This measures it.
//
// Per query (a note summary, with that note's OWN project as the context), four
// top-8 sets: baseline (no graph, no prior), graph (default), context (prior
// only, graph off), both. Then, over queries where the prior actually changed
// the top-8:
//   OVERLAP  = mean Jaccard(items the prior promotes over baseline,
//                           items the graph leg promotes over baseline)
//              -- high means the prior re-ranks what the graph leg already does.
//   CTX-ADDS = mean top-8 items the prior adds ON TOP of the graph leg
//              -- ~0 means the prior is subsumed by the graph leg.
// Split by query project, since the effect concentrates on the minority project.
//
// Gated on COGNOSIS_GRAPHTUNE_DSN (an isolated MULTI-PROJECT dump); skipped in CI.
func TestContextPrior(t *testing.T) {
	dsn := os.Getenv("COGNOSIS_GRAPHTUNE_DSN")
	if dsn == "" {
		t.Skip("set COGNOSIS_GRAPHTUNE_DSN to an isolated multi-project real-vault dump")
	}
	ctx := context.Background()

	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	url := envOr("OLLAMA_URL", "http://localhost:11434")
	model := envOr("OLLAMA_MODEL", "nomic-embed-text:v1.5")
	table := "embeddings_ollama_nomic_embed_text_v1_5"
	prov := embed.NewOllama(url, model)
	if err := prov.Health(ctx); err != nil {
		t.Fatalf("ollama health: %v", err)
	}
	e := &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: prov, Table: table}}}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, "select summary, coalesce(project,'') from notes where summary <> '' order by path")
	if err != nil {
		t.Fatal(err)
	}
	type q struct{ text, project string }
	var queries []q
	projCount := map[string]int{}
	for rows.Next() {
		var qq q
		if err := rows.Scan(&qq.text, &qq.project); err != nil {
			t.Fatal(err)
		}
		queries = append(queries, qq)
		projCount[qq.project]++
	}
	rows.Close()
	if len(queries) == 0 {
		t.Fatal("no note summaries to query with")
	}

	topSet := func(res []query.Result) map[string]bool {
		m := map[string]bool{}
		top := res
		if len(top) > query.DefaultTopK {
			top = top[:query.DefaultTopK]
		}
		for _, r := range top {
			m[r.Content] = true
		}
		return m
	}
	minus := func(a, b map[string]bool) map[string]bool {
		out := map[string]bool{}
		for k := range a {
			if !b[k] {
				out[k] = true
			}
		}
		return out
	}
	jaccard := func(a, b map[string]bool) float64 {
		if len(a) == 0 && len(b) == 0 {
			return 1
		}
		inter := 0
		for k := range a {
			if b[k] {
				inter++
			}
		}
		union := len(a) + len(b) - inter
		if union == 0 {
			return 1
		}
		return float64(inter) / float64(union)
	}

	// project scope label for a query
	scopeOf := func(p string) string {
		if p == "" {
			return "<global>"
		}
		return p
	}

	weights := []float64{1.5, 2.0, 3.0}
	var b strings.Builder
	fmt.Fprintf(&b, "context-prior (encoding-specificity) sweep -- REAL multi-project dump, %d notes queried by summary\n", len(queries))
	fmt.Fprintf(&b, "projects: ")
	for p, c := range projCount {
		fmt.Fprintf(&b, "%s=%d ", scopeOf(p), c)
	}
	b.WriteString("\n\nEach query uses its OWN note's project as the context. CTX-ACTS = queries the\n" +
		"prior changed the top-8 of. OVERLAP = mean Jaccard(prior-promoted, graph-promoted)\n" +
		"over acting queries -- high => the prior re-ranks what the graph leg already does.\n" +
		"CTX-ADDS = mean top-8 items the prior adds ON TOP of the graph leg (~0 => subsumed).\n\n")
	fmt.Fprintf(&b, "%-7s %-11s %9s %8s %9s\n", "WEIGHT", "SCOPE", "CTX-ACTS", "OVERLAP", "CTX-ADDS")

	for _, w := range weights {
		// accumulators keyed by scope ("all" plus each project)
		type acc struct {
			acts, total int
			sumOverlap  float64
			sumAdds     float64
		}
		accs := map[string]*acc{}
		get := func(k string) *acc {
			if accs[k] == nil {
				accs[k] = &acc{}
			}
			return accs[k]
		}

		for _, qq := range queries {
			e.Tuning = query.Tuning{DisableGraph: true}
			base, err := e.Run(ctx, qq.text, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			e.Tuning = query.Tuning{}
			graph, err := e.Run(ctx, qq.text, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			ctxTune := query.Tuning{DisableGraph: true}
			bothTune := query.Tuning{}
			if qq.project != "" {
				ctxTune.ContextProject, ctxTune.ContextWeight = qq.project, w
				bothTune.ContextProject, bothTune.ContextWeight = qq.project, w
			}
			e.Tuning = ctxTune
			cres, err := e.Run(ctx, qq.text, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			e.Tuning = bothTune
			bres, err := e.Run(ctx, qq.text, query.Options{})
			if err != nil {
				t.Fatal(err)
			}

			bs, gs, cs, bo := topSet(base), topSet(graph), topSet(cres), topSet(bres)
			ctxPromoted := minus(cs, bs)   // items the prior lifts into top-8
			graphPromoted := minus(gs, bs) // items the graph leg lifts into top-8
			ctxAdds := len(minus(bo, gs))  // items the prior adds on top of the graph leg
			acted := jaccardChanged(cs, bs)

			for _, k := range []string{"all", scopeOf(qq.project)} {
				a := get(k)
				a.total++
				if acted {
					a.acts++
					a.sumOverlap += jaccard(ctxPromoted, graphPromoted)
					a.sumAdds += float64(ctxAdds)
				}
			}
		}

		order := []string{"all", "cognosis", "analytica", "<global>"}
		for _, k := range order {
			a := accs[k]
			if a == nil || a.total == 0 {
				continue
			}
			overlap, adds := 0.0, 0.0
			if a.acts > 0 {
				overlap = a.sumOverlap / float64(a.acts)
				adds = a.sumAdds / float64(a.acts)
			}
			fmt.Fprintf(&b, "%-7.1f %-11s %6d/%-3d %8.3f %9.2f\n", w, k, a.acts, a.total, overlap, adds)
		}
	}
	e.Tuning = query.Tuning{}

	out := b.String()
	t.Log("\n" + out)
	if err := os.WriteFile(filepath.Join("testdata", "context_prior_sweep.txt"), []byte(out), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

// jaccardChanged reports whether two top-8 content sets differ at all.
func jaccardChanged(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return true
	}
	for k := range a {
		if !b[k] {
			return true
		}
	}
	return false
}
