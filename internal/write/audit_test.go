package write

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/store"
)

// TestAuditGraphDetectsAMissingEdge -- the graph is the one part of the index
// that can decay while notes, chunks and embeddings all stay correct, because
// links are resolved once at index time and never re-derived. Deleting an edge
// directly is exactly what the failures this audit exists for leave behind:
// an atomic editor save cascaded inbound links away, and reconciliation then
// skipped the referrer forever because its content hash had not changed.
func TestAuditGraphDetectsAMissingEdge(t *testing.T) {
	p, s, _, ctx := testPipeline(t)

	if err := p.Write(ctx, "entries/target.md", noteContentNoID("the target\n"), ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, "entries/src.md", noteContentNoID("points at [[target]]\n"), ""); err != nil {
		t.Fatal(err)
	}

	g, err := p.Indexer.AuditGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !g.OK() {
		t.Fatalf("healthy index reported as degraded: %+v", g)
	}
	if g.Edges != 1 || g.Notes != 2 {
		t.Fatalf("audit = %d edges / %d notes, want 1/2", g.Edges, g.Notes)
	}

	// Drop the edge the way a cascade would, leaving content untouched.
	src, err := s.GetNote(ctx, "entries/src.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLinks(ctx, src.ID, nil); err != nil {
		t.Fatal(err)
	}

	g, err = p.Indexer.AuditGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.OK() {
		t.Fatal("audit reports healthy with an edge deleted -- it cannot see the failure it exists for")
	}
	if g.Missing != 1 {
		t.Errorf("missing = %d, want 1", g.Missing)
	}
	if len(g.Sample) == 0 || g.Sample[0] != "entries/src.md" {
		t.Errorf("sample does not name the offending note: %v", g.Sample)
	}
}

// An edge the content does not imply is the opposite corruption, and must not
// be silently tolerated -- a stale edge keeps a note reachable through a link
// its author removed.
func TestAuditGraphDetectsAnExtraEdge(t *testing.T) {
	p, s, _, ctx := testPipeline(t)

	if err := p.Write(ctx, "entries/a.md", noteContentNoID("no links here\n"), ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, "entries/b.md", noteContentNoID("also none\n"), ""); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetNote(ctx, "entries/a.md")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.GetNote(ctx, "entries/b.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLinks(ctx, a.ID, []store.Link{{Dst: b.ID, Kind: "wikilink"}}); err != nil {
		t.Fatal(err)
	}

	g, err := p.Indexer.AuditGraph(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.Extra != 1 {
		t.Errorf("extra = %d, want 1 (an edge no content implies)", g.Extra)
	}
}
