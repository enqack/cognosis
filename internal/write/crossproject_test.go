package write

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
)

func putNote(t *testing.T, ctx context.Context, ix *Indexer, rel, project, extraFM, body string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	fm := "---\nid: " + id.String() + "\ncategory: entry\n"
	if project != "" {
		fm += "project: " + project + "\n"
	}
	fm += extraFM
	fm += "created: \"2026-07-13 09:00:00\"\nupdated: \"2026-07-13 09:00:00\"\n---\n" + body + "\n"
	n, err := vault.ParseNote(rel, []byte(fm))
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Index(ctx, n, FileMeta{Mtime: time.Now(), Size: 1, Blake3: rel}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestCrossProjectLinkQualifier -- two projects share the basename "shared"
// (same file basename in different directories). A qualified
// [[project:basename]] link picks that project's note even when it is NOT the
// unqualified first-wins winner; an unqualified link keeps first-wins.
func TestCrossProjectLinkQualifier(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	ix := &Indexer{Store: s} // links only; no embeddings needed

	// Path order makes beta's note the unqualified first-wins winner
	// ("entries/beta/shared.md" sorts before "entries/zulu/shared.md").
	betaShared := putNote(t, ctx, ix, "entries/beta/shared.md", "beta", "", "beta's shared note")
	alphaShared := putNote(t, ctx, ix, "entries/zulu/shared.md", "alpha", "", "alpha's shared note")

	srcID := putNote(t, ctx, ix, "entries/citer.md", "", "",
		"See [[alpha:shared]] for alpha's take and [[shared]] for the default.")

	dsts, err := s.LinkDsts(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, d := range dsts {
		got[d] = true
	}
	if len(got) != 2 {
		t.Fatalf("links = %d, want 2 (qualified alpha + unqualified first-wins beta)", len(got))
	}
	if !got[alphaShared] {
		t.Fatal("[[alpha:shared]] did not resolve to alpha's note despite beta winning unqualified")
	}
	if !got[betaShared] {
		t.Fatal("[[shared]] did not keep first-wins resolution")
	}

	// A qualifier that matches nothing dangles silently, like any other
	// unresolvable link.
	src2 := putNote(t, ctx, ix, "entries/citer2.md", "", "", "See [[gamma:shared]].")
	if n, _ := s.CountLinks(ctx, src2); n != 0 {
		t.Fatalf("dangling qualified link created %d edges", n)
	}
}

// TestSummaryRoundTrip -- a frontmatter summary lands on the note row and is
// carried through listing.
func TestSummaryRoundTrip(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	ix := &Indexer{Store: s}

	putNote(t, ctx, ix, "entries/summarized.md", "",
		"summary: One dry line about the note.\n", "body")

	row, err := s.GetNote(ctx, "entries/summarized.md")
	if err != nil {
		t.Fatal(err)
	}
	if row.Summary != "One dry line about the note." {
		t.Fatalf("summary = %q", row.Summary)
	}
	metas, err := s.ListNotes(ctx, "")
	if err != nil || len(metas) != 1 || metas[0].Summary != "One dry line about the note." {
		t.Fatalf("list summary = %+v (%v)", metas, err)
	}
	_ = store.Note{} // keep the store import stable if assertions shrink
}
