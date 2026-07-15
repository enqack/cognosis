package store

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// testStore migrates and connects inside a per-run isolated schema, dropped on
// cleanup — tests are parallel-safe and leave nothing behind. Needs a real
// Postgres via COGNOSIS_TEST_DSN (the dev shell provides one); skips otherwise.
func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	base := os.Getenv("COGNOSIS_TEST_DSN")
	if base == "" {
		t.Skip("COGNOSIS_TEST_DSN not set; store tests need a real Postgres (run pg-start in the dev shell)")
	}
	ctx := context.Background()
	var suffix [8]byte
	if _, err := crand.Read(suffix[:]); err != nil {
		t.Fatalf("random schema suffix: %v", err)
	}
	schema := fmt.Sprintf("cog_test_%d_%d", os.Getpid(), binary.BigEndian.Uint64(suffix[:]))

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`create schema %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf(`drop schema %q cascade`, schema))
		_ = admin.Close(ctx)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("options", fmt.Sprintf("-csearch_path=%s,public", schema))
	u.RawQuery = q.Encode()
	dsn := u.String()

	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s, ctx
}

func testNote(path string) Note {
	now := time.Now().UTC().Truncate(time.Second)
	return Note{
		Path:     path,
		ID:       uuid.New(),
		Project:  "cognosis",
		Category: "concept",
		Status:   "active",
		Created:  now,
		Updated:  now,
		Frontmatter: map[string]any{
			"id": "x", "category": "concept",
		},
		Content: "body of " + path,
		Mtime:   now,
		Size:    42,
		Blake3:  "deadbeef",
	}
}

func TestUpsertGetRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	n := testNote("notes/alpha.md")
	if err := s.UpsertNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNote(ctx, n.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != n.ID || got.Content != n.Content || got.Category != n.Category {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, n)
	}
}

func TestMoveDetectedByID(t *testing.T) {
	s, ctx := testStore(t)
	n := testNote("notes/old.md")
	if err := s.UpsertNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	n.Path = "notes/new.md"
	if err := s.UpsertNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, "notes/old.md"); err == nil {
		t.Fatal("old path should be gone after move")
	}
	if _, err := s.GetNote(ctx, "notes/new.md"); err != nil {
		t.Fatalf("new path missing: %v", err)
	}
}

func TestChunksCascadeAndRebuild(t *testing.T) {
	s, ctx := testStore(t)
	n := testNote("notes/beta.md")
	if err := s.UpsertNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	chunks := []Chunk{
		{Ordinal: 0, HeadingPath: "h1", Content: "first", ContentHash: "c0"},
		{Ordinal: 1, HeadingPath: "h1/h2", Content: "second", ContentHash: "c1"},
	}
	if err := s.ReplaceChunks(ctx, n.Path, chunks); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountChunks(ctx, n.Path); got != 2 {
		t.Fatalf("chunks = %d, want 2", got)
	}

	// Derived/droppable proof: wipe chunks, rebuild purely from notes.content.
	if err := s.ReplaceChunks(ctx, n.Path, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountChunks(ctx, n.Path); got != 0 {
		t.Fatalf("chunks after drop = %d, want 0", got)
	}
	stored, err := s.GetNote(ctx, n.Path)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := []Chunk{{Ordinal: 0, Content: stored.Content, ContentHash: "r0"}}
	if err := s.ReplaceChunks(ctx, n.Path, rebuilt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountChunks(ctx, n.Path); got != 1 {
		t.Fatalf("chunks after rebuild = %d, want 1", got)
	}

	// Cascade proof: deleting the note takes its chunks with it.
	if err := s.DeleteNote(ctx, n.Path); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountChunks(ctx, n.Path); got != 0 {
		t.Fatalf("chunks after note delete = %d, want 0", got)
	}
}

func TestLinksKeyedOnStableID(t *testing.T) {
	s, ctx := testStore(t)
	a, b := testNote("notes/a.md"), testNote("notes/b.md")
	for _, n := range []Note{a, b} {
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetLinks(ctx, a.ID, []Link{{Dst: b.ID, Kind: "wikilink"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountLinks(ctx, a.ID); got != 1 {
		t.Fatalf("links = %d, want 1", got)
	}

	// A move must not orphan the edge (links reference id, not path).
	b2 := b
	b2.Path = "notes/b-moved.md"
	if err := s.UpsertNote(ctx, b2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountLinks(ctx, a.ID); got != 1 {
		t.Fatalf("links after dst move = %d, want 1", got)
	}
}

func TestFileStates(t *testing.T) {
	s, ctx := testStore(t)
	n := testNote("entries/2026-07-12.md")
	if err := s.UpsertNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	states, err := s.FileStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := states[n.Path]
	if !ok {
		t.Fatal("path missing from file states")
	}
	if st.Size != n.Size || st.Blake3 != n.Blake3 {
		t.Fatalf("state mismatch: %+v", st)
	}
}

func TestSchemaStatusCurrent(t *testing.T) {
	base := os.Getenv("COGNOSIS_TEST_DSN")
	if base == "" {
		t.Skip("COGNOSIS_TEST_DSN not set")
	}
	// Reuse the isolated-schema plumbing solely for its migrated DSN.
	_, _ = testStore(t) // ensures migrations run cleanly at least once this process
	st, err := Status(base)
	// base itself may or may not be migrated; only assert the call works and
	// reports a sane latest version.
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Latest < 1 {
		t.Fatalf("latest = %d, want >= 1", st.Latest)
	}
}
