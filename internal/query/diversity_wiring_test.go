package query_test

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/write"
)

// The fan-effect diversity penalty must reach Run and actually free top-K slots
// a single over-associated note would otherwise own. Own corpus so the shared
// golden fixture is untouched.
//
// crowder.md has five sections that all match the query, so its chunks take the
// whole top-3 by default; other1/other2 each contribute one matching chunk but
// never place. With the penalty, crowder's redundant chunks are demoted and the
// two other notes rise -- the distinct-source count of the answer goes 1 -> 3.
func TestDiversityPenaltyReachesRun(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	bare := &write.Indexer{Store: s} // keyword leg only; no vector/graph to confound

	const filler = "This paragraph carries enough neutral prose to keep the section above the " +
		"chunker merge floor so that it stays its own chunk rather than folding into a neighbour."
	sec := func(h string) string {
		return "## " + h + "\nThe retention window governs the sweep cadence. " + filler + "\n\n"
	}
	index(t, ctx, bare, "entries/crowder.md", "",
		sec("Alpha")+sec("Bravo")+sec("Charlie")+sec("Delta")+sec("Echo"))
	index(t, ctx, bare, "entries/other1.md", "",
		"The retention window governs the sweep cadence. "+filler+"\n")
	index(t, ctx, bare, "entries/other2.md", "",
		"The retention window governs the sweep cadence. "+filler+"\n")

	const q = "retention window sweep cadence"
	e := &query.Engine{Store: s}
	opts := query.Options{TopK: 3}

	// Penalty disabled: the crowder owns the whole top-3.
	e.Tuning = query.Tuning{DiversityDecay: -1}
	off, offStats, err := e.RunWithStats(ctx, q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if offStats.Sources != 1 {
		t.Fatalf("off: top-3 spans %d notes, want 1 (crowder should own it): %v", offStats.Sources, paths(off))
	}

	// Penalty on: the crowder's extras are demoted and both other notes place.
	e.Tuning = query.Tuning{DiversityDecay: 0.3}
	on, onStats, err := e.RunWithStats(ctx, q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if onStats.Sources <= offStats.Sources {
		t.Fatalf("on: top-3 spans %d notes, want more than the %d without the penalty: %v",
			onStats.Sources, offStats.Sources, paths(on))
	}
	ps := paths(on)
	if !contains(ps, "entries/other1.md") || !contains(ps, "entries/other2.md") {
		t.Errorf("penalty did not free slots for the crowded-out notes: %v", ps)
	}
}
