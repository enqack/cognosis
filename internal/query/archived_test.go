package query_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

const archQuery = "the migration architecture decision"

// archivedFixture models the soft-delete leak scenario:
//   - live.md        an active entry about the topic
//   - shelved.md     an archived (faded) note about the same topic, archived
//     2026-05-01, with a dense on-topic body
//   - reflection.md  a dense reflection that links to shelved.md -- the vector
//     that leaks a shelved concept back into context
//
// stale.md's body embeds nearest the query so, unpenalized, it would rank #1.
func archivedFixture(t *testing.T) (*query.Engine, context.Context) {
	t.Helper()
	s, _ := storetest.New(t)
	ctx := context.Background()

	stub := embedtest.New()
	liveBody := "A live account of the migration architecture decision."
	shelvedBody := "A shelved account of the migration architecture decision, now archived."
	reflectionBody := "A vivid deep dive into the migration architecture decision and all its details."
	stub.Vectors = map[string][]float32{
		archQuery:      {1, 0, 0, 0, 0, 0, 0, 0},
		liveBody:       {0.9, 0.436, 0, 0, 0, 0, 0, 0},
		shelvedBody:    {0.95, 0.312, 0, 0, 0, 0, 0, 0},
		reflectionBody: {1, 0, 0, 0, 0, 0, 0, 0}, // best vector match -- would win unpenalized
	}
	table := embed.TableSlug(stub.Name(), stub.Model())
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(), table, stub.Dim, true); err != nil {
		t.Fatal(err)
	}
	ix := &write.Indexer{Store: s, Provider: stub, Table: table}

	putRaw := func(rel, content string) {
		t.Helper()
		n, err := vault.ParseNote(rel, []byte(content))
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		if err := ix.Index(ctx, n, write.FileMeta{Mtime: time.Now(), Size: 1, Blake3: rel}); err != nil {
			t.Fatalf("index %s: %v", rel, err)
		}
	}

	putRaw("entries/live.md", "---\nid: "+uuid.NewString()+"\ncategory: entry\n"+
		"created: \"2026-01-01 09:00:00\"\nupdated: \"2026-01-01 09:00:00\"\n---\n"+liveBody+"\n")

	// A note that has been soft-deleted in place (status faded + archived_at).
	putRaw("archive/shelved.md", "---\nid: "+uuid.NewString()+"\ncategory: entry\n"+
		"created: \"2026-01-01 09:00:00\"\nupdated: \"2026-01-01 09:00:00\"\n"+
		"status: faded\narchived_at: \"2026-05-01 00:00:00\"\n---\n"+shelvedBody+"\n")

	// A reflection that references the shelved note by wikilink -- its dense body
	// is the leak vector the RRF penalty must catch.
	putRaw("reflections/reflection.md", "---\nid: "+uuid.NewString()+"\ncategory: reflection\n"+
		"persona: deep-thoughts\n"+
		"created: \"2026-01-01 09:00:00\"\nupdated: \"2026-01-01 09:00:00\"\n---\n"+
		reflectionBody+"\n\nSee [[shelved]] for the original.\n")

	return &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: stub, Table: table}}}, ctx
}

// TestArchivedNoteExcludedByDefault -- takeaway #3a: an archived note's own
// chunks are out of ordinary retrieval, but include_archived surfaces them.
func TestArchivedNoteExcludedByDefault(t *testing.T) {
	e, ctx := archivedFixture(t)

	rs, err := e.Run(ctx, archQuery, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if has(rs, "archive/shelved.md") {
		t.Fatalf("archived note leaked into default retrieval: %v", paths(rs))
	}

	rs, err = e.Run(ctx, archQuery, query.Options{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if !has(rs, "archive/shelved.md") {
		t.Fatalf("include_archived did not surface the archived note: %v", paths(rs))
	}
}

// TestArchivedAsOfHonest -- takeaway #3a temporal honesty: before it was
// archived, the note was live and must appear in an as_of view at that instant.
func TestArchivedAsOfHonest(t *testing.T) {
	e, ctx := archivedFixture(t)
	rs, err := e.Run(ctx, archQuery, query.Options{AsOf: asOf(t, "2026-03-01 00:00:00")})
	if err != nil {
		t.Fatal(err)
	}
	if !has(rs, "archive/shelved.md") {
		t.Fatalf("as_of before archival should show the then-live note: %v", paths(rs))
	}
}

// TestArchivedLinkPenaltyDepressesReflection -- takeaway #3b: the reflection has
// the strongest vector match, so without the penalty it ranks #1. Because it
// links to an archived note, the fusion penalty must push the live note above
// it.
func TestArchivedLinkPenaltyDepressesReflection(t *testing.T) {
	e, ctx := archivedFixture(t)
	rs, err := e.Run(ctx, archQuery, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) == 0 {
		t.Fatal("no results")
	}
	livePos, reflPos := -1, -1
	for i, r := range rs {
		switch r.Path {
		case "entries/live.md":
			livePos = i
		case "reflections/reflection.md":
			reflPos = i
		}
	}
	if livePos == -1 {
		t.Fatalf("live note missing from results: %v", paths(rs))
	}
	if reflPos != -1 && reflPos < livePos {
		t.Fatalf("penalized reflection outranked the live note: %v", paths(rs))
	}
}
