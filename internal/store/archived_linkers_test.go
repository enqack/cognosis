package store

import (
	"testing"

	"github.com/google/uuid"
)

// TestArchivedLinkers proves the fusion-penalty predicate is precise: a note
// linking to a soft-deleted note is flagged, a note linking only to live notes
// is not, and an empty input short-circuits.
func TestArchivedLinkers(t *testing.T) {
	s, ctx := testStore(t)

	archived := testNote("archive/shelved.md")
	archived.Status = "archived"
	live := testNote("notes/live.md")
	linkerToArchived := testNote("reflections/refA.md")
	linkerToLive := testNote("reflections/refB.md")
	for _, n := range []Note{archived, live, linkerToArchived, linkerToLive} {
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetLinks(ctx, linkerToArchived.ID, []Link{{Dst: archived.ID, Kind: "wikilink"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLinks(ctx, linkerToLive.ID, []Link{{Dst: live.ID, Kind: "wikilink"}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ArchivedLinkers(ctx, []uuid.UUID{linkerToArchived.ID, linkerToLive.ID, live.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !got[linkerToArchived.ID] {
		t.Fatal("note linking to an archived note was not flagged")
	}
	if got[linkerToLive.ID] {
		t.Fatal("note linking only to a live note was wrongly flagged")
	}
	if got[live.ID] {
		t.Fatal("note with no outbound links was wrongly flagged")
	}

	empty, err := s.ArchivedLinkers(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty input should yield empty map, got %v", empty)
	}
}
