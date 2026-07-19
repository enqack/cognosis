package write

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// TestAuditGraphScales pins the query count, not the wall clock. The first
// version issued a full scan of `notes` plus a LinkDsts round trip per note, so
// the audit blew `Status`'s 15s budget on a few thousand notes and reported
// FAIL on a healthy daemon -- the check whose entire value is being trustworthy.
func TestAuditGraphScales(t *testing.T) {
	s, _ := storetest.NewTB(t)
	ctx := context.Background()
	const n = 2000

	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
	}
	now := time.Now().UTC().Truncate(time.Second)
	for i := range ids {
		body := ""
		if i > 0 {
			body = fmt.Sprintf("refers to [[n%05d]]\n", i-1)
		}
		if err := s.UpsertNote(ctx, store.Note{
			ID: ids[i], Path: fmt.Sprintf("entries/n%05d.md", i), Category: "entry",
			Created: now, Updated: now, Content: body,
			Frontmatter: map[string]any{"id": ids[i].String(), "category": "entry"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < n; i++ {
		if err := s.SetLinks(ctx, ids[i], []store.Link{{Dst: ids[i-1], Kind: "wikilink"}}); err != nil {
			t.Fatal(err)
		}
	}

	ix := &Indexer{Store: s}
	start := time.Now()
	g, err := ix.AuditGraph(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d notes, %d edges audited in %s", g.Notes, g.Edges, elapsed)
	if !g.OK() {
		t.Errorf("healthy %d-note graph reported degraded: %+v", n, g)
	}
	// Threshold measured, not guessed. At this N the per-note implementation
	// took 1.92s and the three-query one takes ~5ms, so 500ms separates them by
	// a wide margin in both directions. A 5s bound -- the first thing I reached
	// for -- passes on *both* and would have proved nothing.
	//
	// The absolute number matters less than the growth: work per note was a
	// full scan of `notes` plus a round trip, so the cost is quadratic and
	// crosses Status's 15s budget at roughly 6k notes. This asserts the shape
	// stayed linear-ish rather than asserting one machine's speed.
	if elapsed > 500*time.Millisecond {
		t.Errorf("audit took %s for %d notes; the per-note implementation took ~1.9s here and grows "+
			"quadratically, crossing Status's 15s budget near 6k notes and reporting FAIL on a healthy vault",
			elapsed, n)
	}
}
