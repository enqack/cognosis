package retrievaleval

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// Q4: what does the fix cost? Consumed by a human via benchstat; nothing here
// asserts on wall-clock. Run with `mage bench`, never with -race (race
// instrumentation makes latency numbers meaningless).
//
// The corpus is built once per benchmark function, outside the timed loop —
// index construction otherwise dominates every measurement.

func BenchmarkVectorLeg(b *testing.B) {
	requireEval(b)
	ctx := context.Background()
	c := Build(b, evalSpec(b))
	vec, err := c.Provider.EmbedQuery(ctx, c.Queries[0].Text)
	if err != nil {
		b.Fatal(err)
	}

	for _, scope := range c.ScopeNames() {
		filter := c.Scopes()[scope]
		for _, gs := range gucSettings {
			b.Run(scope+"/"+gs.Name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, 50, gs.Set, false); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkFTSLeg(b *testing.B) {
	requireEval(b)
	ctx := context.Background()
	c := Build(b, evalSpec(b))
	text := c.Queries[0].Text
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Store.ProbeFTS(ctx, text, store.Filter{}, 50, nil, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunEndToEnd is the number that actually matters: fan-out plus
// fusion, default scan settings versus corrected. The delta is the cost of the
// fix as an agent would experience it.
func BenchmarkRunEndToEnd(b *testing.B) {
	requireEval(b)
	ctx := context.Background()
	c := Build(b, evalSpec(b))

	corrected := store.SessionSettings{
		"hnsw.ef_search": "200", "hnsw.iterative_scan": "relaxed_order",
	}
	fixedStore, err := store.Connect(ctx, evalDSN(b, c.DSN, corrected))
	if err != nil {
		b.Fatal(err)
	}
	defer fixedStore.Close()
	fixedEngine := &query.Engine{
		Store:     fixedStore,
		Providers: []query.ProviderLeg{{Provider: c.Provider, Table: c.Table}},
	}

	for _, tc := range []struct {
		name string
		eng  *query.Engine
	}{
		{"default", c.Engine},
		{"corrected", fixedEngine},
	} {
		for _, scope := range []struct {
			name string
			opts query.Options
		}{
			{"all", query.Options{}},
			{"wide", query.Options{Project: "wide"}},
		} {
			b.Run(tc.name+"/"+scope.name, func(b *testing.B) {
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					q := c.Queries[i%len(c.Queries)]
					i++
					if _, err := tc.eng.Run(ctx, q.Text, scope.opts); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
