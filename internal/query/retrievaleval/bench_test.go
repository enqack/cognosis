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
// The corpus is built once per benchmark function, outside the timed loop --
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
// fusion, pre-fix scan settings versus shipped. The delta is the cost of the
// fix as an agent would experience it.
//
// The baseline arm must be built with ConnectWithScanSettings, not by pushing
// GUCs through the DSN: AfterConnect runs after the startup packet and
// overwrites them, which silently made both arms identical and this benchmark
// report a ~0 delta. c.Engine is the *shipped* configuration, not a default.
func BenchmarkRunEndToEnd(b *testing.B) {
	requireEval(b)
	ctx := context.Background()
	c := Build(b, evalSpec(b))

	baseStore, err := store.ConnectWithScanSettings(ctx, c.DSN, []string{
		"set hnsw.ef_search = 40",
		"set hnsw.iterative_scan = 'off'",
	})
	if err != nil {
		b.Fatal(err)
	}
	defer baseStore.Close()
	baseEngine := &query.Engine{
		Store:     baseStore,
		Providers: []query.ProviderLeg{{Provider: c.Provider, Table: c.Table}},
	}

	// A comparison benchmark whose two arms are configured identically should
	// fail, not quietly report no difference. This is the guard that would
	// have caught the DSN-override bug immediately.
	assertPoolsDiffer(ctx, b, baseStore, c.Store)

	for _, tc := range []struct {
		name string
		eng  *query.Engine
	}{
		{"prefix", baseEngine},
		{"shipped", c.Engine},
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

	// What the OR fallback costs when it actually fires.
	//
	// The arms above cannot show this. Every query in c.Queries returns a full
	// pool of 50 keyword candidates on this corpus, so the fallback never
	// triggers and its cost is a single length comparison -- which is why the
	// recorded baseline says nothing about it. Only the starving set puts the
	// leg in the regime where the OR retry executes.
	//
	// Both arms are the shipped engine over the same queries; they differ only
	// in whether the retry is permitted, so the delta is the second keyword
	// query against Postgres and nothing else.
	if len(c.StarveQueries) == 0 {
		b.Fatal("corpus generated no starving queries; spec.StarveQueries/DistinctivePerChunk are zero")
	}
	noFallback := *c.Engine
	noFallback.Tuning = query.Tuning{FTSFallbackBelow: -1} // negative disables; 0 would mean "unset"

	assertFallbackFires(ctx, b, c)

	for _, tc := range []struct {
		name string
		eng  *query.Engine
	}{
		{"starving/fallback", c.Engine},
		{"starving/no-fallback", &noFallback},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				q := c.StarveQueries[i%len(c.StarveQueries)]
				i++
				if _, err := tc.eng.Run(ctx, q.Text, query.Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// assertFallbackFires is the starving arms' counterpart to assertPoolsDiffer:
// two arms that differ only in a branch neither takes are one arm measured
// twice, and would report the fallback as free rather than unmeasured.
func assertFallbackFires(ctx context.Context, b *testing.B, c *Corpus) {
	b.Helper()
	fired := 0
	for _, q := range c.StarveQueries {
		_, stats, err := c.Engine.RunWithStats(ctx, q.Text, query.Options{})
		if err != nil {
			b.Fatal(err)
		}
		if stats.FTSFallback {
			fired++
		}
	}
	if fired == 0 {
		b.Fatalf("the OR fallback fired on none of %d starving queries: the corpus is not "+
			"starving the keyword leg, so both starving arms below run identical code",
			len(c.StarveQueries))
	}
	b.Logf("fallback fires on %d/%d starving queries", fired, len(c.StarveQueries))
}
