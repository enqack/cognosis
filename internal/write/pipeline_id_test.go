package write

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
)

func noteContentNoID(body string) string {
	return `---
category: entry
project: cognosis
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
` + body
}

// TestOmittedIDIsMinted -- the contract requires a UUIDv7 and the MCP surface
// offers no way to produce one, so every note written through it previously
// needed an out-of-band uuid generator. Omitting the id must now work, and must
// produce an id that satisfies the validator that rejected its absence.
func TestOmittedIDIsMinted(t *testing.T) {
	p, s, root, ctx := testPipeline(t)
	const rel = "entries/minted.md"

	if err := p.Write(ctx, rel, noteContentNoID("Body text.\n"), ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatalf("note not indexed: %v", err)
	}
	if n.ID.Version() != 7 {
		t.Errorf("minted id is v%d, want v7 -- the validator rejects anything else", n.ID.Version())
	}

	// The id must also reach the file: the vault is the source of truth, and a
	// note whose id exists only in the index would not survive a rebuild.
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "id: "+n.ID.String()) {
		t.Errorf("minted id absent from the file on disk:\n%s", b)
	}
}

// TestOmittedIDOnExistingPathReusesID is the load-bearing half.
//
// UpsertNote treats same-path-different-id as an eviction: it deletes the row
// and cascades every inbound link away. So minting unconditionally would make
// "omit the id" a way to silently destroy a note's inbound graph on every
// update -- the same damage an atomic editor save used to cause, by a different
// route. The id must be reused, and the referrer's edge must survive.
func TestOmittedIDOnExistingPathReusesID(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	const target = "entries/target.md"

	if err := p.Write(ctx, target, noteContentNoID("First version.\n"), ""); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetNote(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	// A second note points at it, so there is an inbound edge to lose.
	if err := p.Write(ctx, "entries/referrer.md",
		noteContentNoID("See [[target]] for detail.\n"), ""); err != nil {
		t.Fatal(err)
	}
	referrer, err := s.GetNote(ctx, "entries/referrer.md")
	if err != nil {
		t.Fatal(err)
	}
	if dsts, err := s.LinkDsts(ctx, referrer.ID); err != nil {
		t.Fatal(err)
	} else if len(dsts) != 1 {
		t.Fatalf("precondition: referrer should link to target, got %v", dsts)
	}

	// Overwrite the target, again without an id.
	if err := p.Write(ctx, target, noteContentNoID("Second version.\n"), ""); err != nil {
		t.Fatal(err)
	}
	second, err := s.GetNote(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("id changed on overwrite: %s -> %s; UpsertNote evicts on same-path-different-id "+
			"and cascades inbound links away", first.ID, second.ID)
	}
	dsts, err := s.LinkDsts(ctx, referrer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsts) != 1 || dsts[0] != first.ID {
		// Verified against the un-fixed code: the edge is not dropped, it is
		// re-pointed at the newly minted id, because RepairReferrers resolves
		// wikilinks by basename after the write. So the visible symptom is id
		// churn rather than a missing edge -- the note's identity changes on
		// every update, which is exactly the stability note ids exist to give,
		// and anything holding the old id now refers to a row that was evicted.
		t.Errorf("referrer no longer points at the original note id: got %v, want [%s]", dsts, first.ID)
	}
}

// An explicitly supplied id still wins, and a non-v7 one is still rejected --
// minting is a fallback for an absent value, not a relaxation of the contract.
func TestSuppliedIDIsHonouredAndStillValidated(t *testing.T) {
	p, s, _, ctx := testPipeline(t)

	want := uuid.Must(uuid.NewV7()).String()
	if err := p.Write(ctx, "entries/pinned.md", noteContent(want, "cognosis", "Body.\n"), ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNote(ctx, "entries/pinned.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.ID.String() != want {
		t.Errorf("supplied id %s was replaced by %s", want, n.ID)
	}

	v4 := uuid.Must(uuid.NewRandom()).String()
	err = p.Write(ctx, "entries/v4.md", noteContent(v4, "cognosis", "Body.\n"), "")
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("a v4 id was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "UUIDv7") {
		t.Errorf("rejection does not name the requirement: %v", err)
	}
}

// TestEditReplacesUniqueOccurrence -- the ordinary case: change part of a note
// without resending it, and land through the same pipeline as a full write.

func TestIDChangeOnExistingPathRejected(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	const rel = "entries/pinned.md"

	if err := p.Write(ctx, rel, noteContentNoID("first\n"), ""); err != nil {
		t.Fatal(err)
	}
	original, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}

	other := uuid.Must(uuid.NewV7()).String()

	// Via write_note.
	err = p.Write(ctx, rel, noteContent(other, "", "second\n"), "")
	if !cogerr.Is(err, cogerr.Conflict) {
		t.Fatalf("write with a different id: err = %v, want Conflict", err)
	}
	if !strings.Contains(err.Error(), original.ID.String()) {
		t.Errorf("refusal does not name the existing id: %v", err)
	}

	// Via edit_note, which is the newly reachable route.
	err = p.Edit(ctx, rel, "id: "+original.ID.String(), "id: "+other)
	if !cogerr.Is(err, cogerr.Conflict) {
		t.Fatalf("edit changing the id: err = %v, want Conflict", err)
	}

	after, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != original.ID {
		t.Errorf("id changed despite the refusal: %s -> %s", original.ID, after.ID)
	}
}

// Re-supplying the *same* id must still work -- the guard rejects a change, not
// the presence of an id, and round-tripping a note's own frontmatter is the
// most ordinary thing a caller does.
func TestSameIDOnExistingPathAccepted(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	const rel = "entries/same.md"

	if err := p.Write(ctx, rel, noteContentNoID("first\n"), ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, rel, noteContent(n.ID.String(), "", "second\n"), ""); err != nil {
		t.Fatalf("re-supplying the note's own id was rejected: %v", err)
	}
	after, _ := s.GetNote(ctx, rel)
	if after.ID != n.ID || !strings.Contains(after.Content, "second") {
		t.Errorf("write did not land: id %s, content %q", after.ID, after.Content)
	}
}

// TestBlankIDKeyTreatedAsAbsent -- `id:` with no value means the same thing as
// no id line, and it is one of the most natural ways an agent writes "not
// filled in yet". Splicing a second key over it produced two `id` mappings and
// YAML rejected the write with `mapping key "id" already defined at line 1` --
// an error naming a line the caller never wrote, firing on the exact case the
// minting feature exists to serve.
func TestBlankIDKeyTreatedAsAbsent(t *testing.T) {
	p, s, root, ctx := testPipeline(t)

	for _, c := range []struct{ name, idLine string }{
		{"empty", "id:"},
		{"trailing space", "id: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			rel := "entries/blank-" + strings.ReplaceAll(c.name, " ", "-") + ".md"
			content := "---\n" + c.idLine + "\ncategory: entry\n" +
				"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n---\nbody\n"
			if err := p.Write(ctx, rel, content, ""); err != nil {
				t.Fatalf("blank id rejected: %v", err)
			}
			n, err := s.GetNote(ctx, rel)
			if err != nil {
				t.Fatal(err)
			}
			if n.ID.Version() != 7 {
				t.Errorf("minted id is v%d, want v7", n.ID.Version())
			}
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(b), "id:"); got != 1 {
				t.Errorf("file has %d id keys, want 1:\n%s", got, b)
			}
		})
	}
}

// An `id:` nested inside another mapping, or appearing in the body, must
// survive -- dropBlankIDKey is scoped to top-level frontmatter keys.
func TestBlankIDDropDoesNotTouchNestedOrBody(t *testing.T) {
	p, _, root, ctx := testPipeline(t)
	const rel = "entries/nested.md"
	content := "---\ncategory: entry\n" +
		"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n" +
		"meta:\n  id:\n---\n```\nid:\n```\n"
	if err := p.Write(ctx, rel, content, ""); err != nil {
		t.Fatalf("write rejected: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "  id:") {
		t.Errorf("nested id key was stripped:\n%s", b)
	}
	if !strings.Contains(string(b), "```\nid:\n```") {
		t.Errorf("body id line was stripped:\n%s", b)
	}
}
