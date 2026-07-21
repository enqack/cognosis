package query_test

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/write"
)

// The note-level fallback must be reached by Run, be preferred over the bare OR
// floor, and announce which arm fired -- none of which the accessor tests can
// see. This uses its own corpus rather than the shared fixture so the golden
// ranking stays untouched.
//
// Corpus, queried with "special index stored":
//   - split.md scatters the three terms across two H2 sections, so no single
//     chunk holds all of them: per-chunk AND matches nothing, but the note
//     AND-matches at the note level.
//   - decoy.md carries only "stored". A bare OR admits it (one incidental term);
//     note-level membership does not (the note lacks "special" and "index").
//
// So the arm that fired is observable from a single note: when note-level clears
// the threshold it is taken and OR never runs, so decoy.md is ABSENT; when
// note-level is disabled the chain falls through to OR and decoy.md APPEARS.
func TestNoteLevelFallbackReachesRun(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	bare := &write.Indexer{Store: s} // keyword + graph only; no vector leg to confound

	// Each section must clear the chunker's 200-char mergeBelow floor, or the two
	// fold into one chunk that holds every term and per-chunk AND matches it --
	// defeating the whole premise. The filler is deliberately neutral (no query
	// term, no shared vocabulary) so only the planted terms decide membership.
	const filler = "This paragraph carries enough neutral prose to keep the section " +
		"above the chunker merge floor so that it remains its own chunk and does " +
		"not fold into a neighbour, which would otherwise gather every planted term."
	index(t, ctx, bare, "entries/split.md", "",
		"## Origins\nThe special index was introduced in this section. "+filler+"\n\n"+
			"## Storage\nEverything here is stored for later. "+filler+"\n")
	index(t, ctx, bare, "entries/decoy.md", "",
		"Backups are stored every night without fail. "+filler+"\n")

	const q = "special index stored"
	e := &query.Engine{Store: s} // no providers: the keyword leg is the whole story

	// Default engine: note-level is the production fallback.
	got, stats, err := e.RunWithStats(ctx, q, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ps := paths(got)
	if !contains(ps, "entries/split.md") {
		t.Fatalf("note-level fallback did not surface the scattered note: %v", ps)
	}
	if contains(ps, "entries/decoy.md") {
		t.Errorf("decoy.md present under note-level membership: the leg admitted a note "+
			"that only shares one incidental term, so OR ran when it should not have: %v", ps)
	}
	if stats.FTSFallbackKind != query.FTSFallbackNoteLevel {
		t.Errorf("FTSFallbackKind = %q, want %q (fts_and=%d fts=%d)",
			stats.FTSFallbackKind, query.FTSFallbackNoteLevel, stats.FTSPrimary, stats.FTS)
	}

	// Disable note-level: the chain must fall through to the OR floor, which
	// admits decoy.md and reports the OR arm.
	e.Tuning = query.Tuning{DisableNoteLevel: true}
	got, stats, err = e.RunWithStats(ctx, q, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ps = paths(got)
	if !contains(ps, "entries/decoy.md") {
		t.Errorf("with note-level disabled the OR floor should admit decoy.md, but it is "+
			"absent -- the fallback chain did not reach OR: %v", ps)
	}
	if stats.FTSFallbackKind != query.FTSFallbackOr {
		t.Errorf("FTSFallbackKind = %q, want %q with note-level disabled",
			stats.FTSFallbackKind, query.FTSFallbackOr)
	}

	// Disable the fallback entirely: only the AND conjunction runs, which no
	// chunk satisfies, so the scattered note is unreachable. This is what makes
	// the two cases above a wiring proof and not an assertion the feature's
	// removal would still pass.
	e.Tuning = query.Tuning{FTSFallbackBelow: -1}
	got, _, err = e.RunWithStats(ctx, q, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if contains(paths(got), "entries/split.md") {
		t.Errorf("split.md reachable with the fallback disabled: per-chunk AND cannot "+
			"match terms scattered across chunks, so something else surfaced it: %v", paths(got))
	}
}
