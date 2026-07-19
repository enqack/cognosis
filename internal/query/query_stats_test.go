package query_test

import (
	"context"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/write"
)

func TestCountSuppressedFalsified(t *testing.T) {
	_, dsn, ctx := fixture(t)
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// moon.md is the fixture's falsified note; its body is moonBody.
	n, err := s.CountSuppressedFalsified(ctx, "moon", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (the falsified moon note matches 'moon')", n)
	}

	// Nothing is being suppressed when the caller already asked for them --
	// reporting a count there would tell the agent to do what it just did.
	n, err = s.CountSuppressedFalsified(ctx, "moon", store.Filter{IncludeFalsified: true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count = %d with include_falsified, want 0", n)
	}

	// A query matching only live notes must not claim suppressed history.
	n, err = s.CountSuppressedFalsified(ctx, "gardening", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count = %d for a query no falsified note matches, want 0", n)
	}
}

// TestRunWithStatsCountsEachLeg -- the counts exist to drive a decision about
// the keyword leg, so a plausible-but-wrong number is worse than none. This
// pins them against the legs the engine actually ran rather than against each
// other.
//
// The specific failure it guards: FTS is written from inside the errgroup
// alongside the vector legs, so a count assigned to the wrong slot, or lost to
// the race between them, would still produce a believable log line.
func TestRunWithStatsCountsEachLeg(t *testing.T) {
	e, _, ctx := fixture(t)

	results, stats, err := e.RunWithStats(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fused < len(results) {
		t.Errorf("fused=%d is below the %d results returned; the fused count is taken before "+
			"the top-K cut and cannot be smaller", stats.Fused, len(results))
	}
	if stats.Vector == 0 && stats.FTS == 0 && stats.Graph == 0 {
		t.Fatal("every leg reported zero candidates yet the query returned results")
	}
	if stats.Fused == 0 {
		t.Error("fused count is zero for a query that returned results")
	}

	// Each leg is counted independently: disabling the graph leg must zero
	// that count and leave the others intact. Asserting only on totals would
	// pass if two legs' counts were swapped.
	graphOn := stats
	e.Tuning = query.Tuning{DisableGraph: true}
	if _, off, err := e.RunWithStats(ctx, queryText, query.Options{}); err != nil {
		t.Fatal(err)
	} else {
		if off.Graph != 0 {
			t.Errorf("graph=%d with DisableGraph set", off.Graph)
		}
		if off.Vector != graphOn.Vector || off.FTS != graphOn.FTS {
			t.Errorf("disabling the graph leg changed the other counts: vector %d->%d, fts %d->%d",
				graphOn.Vector, off.Vector, graphOn.FTS, off.FTS)
		}
	}
}

// Run must stay a thin delegation to RunWithStats -- one implementation, so the
// counts describe the query that actually ran rather than a parallel copy of
// the logic that could drift from it.
func TestRunMatchesRunWithStats(t *testing.T) {
	e, _, ctx := fixture(t)

	plain, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	withStats, _, err := e.RunWithStats(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != len(withStats) {
		t.Fatalf("Run returned %d results, RunWithStats %d", len(plain), len(withStats))
	}
	for i := range plain {
		if plain[i].Path != withStats[i].Path {
			t.Errorf("rank %d: Run=%s RunWithStats=%s", i, plain[i].Path, withStats[i].Path)
		}
	}
}

// TestRunWithStatsCountsDistinctSources -- sources/fused_sources exist to show
// whether one note monopolised an answer, so the only number that carries
// information is the one that *differs* from the chunk count. `Sources =
// len(fused)` would log a plausible value and answer nothing.
//
// The shared fixture cannot catch that: every note there is a single chunk, so
// distinct-notes and chunk-count are equal and the two implementations are
// indistinguishable. This builds a corpus where one note contributes several
// matching chunks, and asserts that the fixture actually achieved that before
// trusting anything measured on it -- a flat "no crowding" result would
// otherwise pass for the wrong reason.
func TestRunWithStatsCountsDistinctSources(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()

	stub := embedtest.New()
	table := embed.TableSlug(stub.Name(), stub.Model())
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(), table, stub.Dim, true); err != nil {
		t.Fatal(err)
	}
	ix := &write.Indexer{Store: s, Provider: stub, Table: table}

	// Sections must clear chunk.mergeBelow (200 bytes) or they merge back into
	// one chunk and the crowding disappears. Each carries the query's terms so
	// the keyword leg returns all three -- crowding via FTS rather than pinned
	// vectors, which keeps the fixture independent of the stub's vector map.
	pad := strings.Repeat("supporting detail about the surrounding machinery. ", 5)
	crowder := "## One\n\nThe index is stored here. " + pad +
		"\n\n## Two\n\nThe index is stored differently. " + pad +
		"\n\n## Three\n\nThe index is stored again. " + pad
	index(t, ctx, ix, "entries/crowder.md", "", crowder)
	index(t, ctx, ix, "entries/short-a.md", "", "The index is stored briefly. "+pad)
	index(t, ctx, ix, "entries/short-b.md", "", "Where the index is stored, concisely. "+pad)

	e := &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: stub, Table: table}}}

	results, stats, err := e.RunWithStats(ctx, queryText, query.Options{TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no results; nothing to count sources over")
	}

	byPath := map[string]int{}
	for _, r := range results {
		byPath[r.Path]++
	}

	// Positive control. If no note placed more than once there is no crowding
	// in this corpus, every count below is trivially consistent, and the test
	// proves nothing about the measurement it exists to check.
	if byPath["entries/crowder.md"] < 2 {
		t.Fatalf("fixture produced no crowding: crowder placed %d chunks of %d results (%v) -- "+
			"the assertions below cannot distinguish a distinct-note count from a chunk count",
			byPath["entries/crowder.md"], len(results), byPath)
	}

	if stats.Sources != len(byPath) {
		t.Errorf("sources=%d but the %d returned chunks span %d distinct notes (%v)",
			stats.Sources, len(results), len(byPath), byPath)
	}
	// The assertion mutant A fails: with crowding present these must differ.
	if stats.Sources >= len(results) {
		t.Errorf("sources=%d is not below the %d chunks returned, yet %d of them came from one note -- "+
			"the count is tracking chunks, not notes",
			stats.Sources, len(results), byPath["entries/crowder.md"])
	}
	if stats.FusedSources < stats.Sources {
		t.Errorf("fused_sources=%d is below sources=%d; the pre-cut pool cannot hold fewer notes than the cut kept",
			stats.FusedSources, stats.Sources)
	}

	// A top-K of 1 must concentrate to one note while the pre-cut pool keeps
	// its diversity -- the cut-concentrates case the pair was added to detect.
	// A FusedSources taken after the cut survives every assertion above.
	narrow, narrowStats, err := e.RunWithStats(ctx, queryText, query.Options{TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow) != 1 {
		t.Fatalf("TopK=1 returned %d results", len(narrow))
	}
	if narrowStats.Sources != 1 {
		t.Errorf("sources=%d for a single-chunk answer", narrowStats.Sources)
	}
	if narrowStats.FusedSources != stats.FusedSources {
		t.Errorf("fused_sources changed with top-K (%d -> %d); it is taken before the cut and must not",
			stats.FusedSources, narrowStats.FusedSources)
	}
}
