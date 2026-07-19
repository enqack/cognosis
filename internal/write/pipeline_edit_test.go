package write

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
)

func TestEditReplacesUniqueOccurrence(t *testing.T) {
	p, s, root, ctx := testPipeline(t)
	const rel = "entries/edit.md"

	if err := p.Write(ctx, rel, noteContentNoID("First version of the body.\n"), ""); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Edit(ctx, rel, "First version", "Second version"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Second version") || strings.Contains(string(b), "First version") {
		t.Errorf("file not edited:\n%s", b)
	}

	after, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Content, "Second version") {
		t.Errorf("index not updated: %q", after.Content)
	}
	// Identity must survive an edit for the same reason it survives a rewrite:
	// UpsertNote evicts on same-path-different-id.
	if after.ID != before.ID {
		t.Errorf("note id changed across an edit: %s -> %s", before.ID, after.ID)
	}
}

// An edit that cannot be applied unambiguously must be refused, not guessed.
// The caller cannot see the file, so picking "the first match" is a decision
// about content they are not looking at.
func TestEditRefusesAmbiguousAndMissing(t *testing.T) {
	p, _, _, ctx := testPipeline(t)
	const rel = "entries/ambig.md"

	body := "the same phrase here\nand the same phrase again\n"
	if err := p.Write(ctx, rel, noteContentNoID(body), ""); err != nil {
		t.Fatal(err)
	}

	err := p.Edit(ctx, rel, "the same phrase", "changed")
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("ambiguous edit: err = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("refusal does not report the match count, so the caller cannot fix it: %v", err)
	}

	err = p.Edit(ctx, rel, "text that is absent", "x")
	if !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("missing old_string: err = %v, want NotFound", err)
	}

	err = p.Edit(ctx, "entries/no-such-note.md", "anything", "x")
	if !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("missing note: err = %v, want NotFound", err)
	}
}

// An edit producing invalid frontmatter must be rejected and leave the note as
// it was. Edit routes through the same validation as Write, so the failure mode
// to guard is a partially-applied change on disk.
func TestEditRejectedLeavesNoteIntact(t *testing.T) {
	p, s, root, ctx := testPipeline(t)
	const rel = "entries/valid.md"

	if err := p.Write(ctx, rel, noteContentNoID("Body.\n"), ""); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	// Remove a required frontmatter key.
	err = p.Edit(ctx, rel, "created: \"2026-07-12 09:00:00\"\n", "")
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("err = %v, want Validation", err)
	}

	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Errorf("a rejected edit modified the file:\n--- before ---\n%s\n--- after ---\n%s", original, after)
	}
	if _, err := s.GetNote(ctx, rel); err != nil {
		t.Errorf("note no longer indexed after a rejected edit: %v", err)
	}
}

// TestConcurrentEditsDoNotLoseOne is why the read and the write share one path
// lock. Read-modify-write without it drops an edit whenever two land together,
// and the loser returns success -- a silent write loss, which is the worst
// available failure for a tool whose job is to record things.
func TestConcurrentEditsDoNotLoseOne(t *testing.T) {
	p, _, root, ctx := testPipeline(t)
	const rel = "entries/concurrent.md"

	if err := p.Write(ctx, rel, noteContentNoID("alpha\nbravo\n"), ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	edits := []struct{ from, to string }{
		{"alpha", "ALPHA"},
		{"bravo", "BRAVO"},
	}
	for i, e := range edits {
		wg.Add(1)
		go func(i int, from, to string) {
			defer wg.Done()
			errs[i] = p.Edit(ctx, rel, from, to)
		}(i, e.from, e.to)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}

	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ALPHA", "BRAVO"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("edit for %q was lost to a concurrent edit that reported success:\n%s", want, b)
		}
	}
}

// TestIDChangeOnExistingPathRejected -- the eviction guard was one-sided: an
// omitted id reused the existing one, but a *supplied* different id still
// evicted the row. edit_note made that a one-call operation on frontmatter the
// caller can see, so the tool added to prevent link damage could cause it.
//
// What actually breaks is identity rather than links: RepairReferrers
// re-resolves wikilinks by basename after the write, so edges follow the new
// id. An id is written once precisely so it survives moves, and anything
// holding the old one names a row that no longer exists.
