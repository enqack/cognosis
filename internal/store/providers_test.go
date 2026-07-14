package store

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestEnsureProviderProvisionsShape — a provider with an unseen dimension gets
// a correctly shaped table, asserted via catalog query, not by using it.
func TestEnsureProviderProvisionsShape(t *testing.T) {
	s, ctx := testStore(t)

	const table = "embeddings_fake_prov_model_x"
	if err := s.EnsureProvider(ctx, "fake-prov", "model-x", table, 123, true); err != nil {
		t.Fatal(err)
	}

	// Catalog assertion: the embedding column is vector(123).
	base := os.Getenv("COGNOSIS_TEST_DSN")
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	var typmod int
	err = s.pool.QueryRow(ctx, `
		select a.atttypmod from pg_attribute a
		join pg_class c on c.oid = a.attrelid
		where c.relname = $1 and a.attname = 'embedding'`, table).Scan(&typmod)
	if err != nil {
		t.Fatalf("catalog lookup: %v", err)
	}
	// For pgvector, atttypmod carries the declared dimension directly.
	if typmod != 123 {
		t.Fatalf("vector dimension = %d, want 123", typmod)
	}

	// Registered and active.
	p, err := s.ActiveProvider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Table != table || p.Dimension != 123 {
		t.Fatalf("active provider = %+v", p)
	}

	// Idempotent re-ensure.
	if err := s.EnsureProvider(ctx, "fake-prov", "model-x", table, 123, true); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
}

// TestActiveProviderIsSingular — activating a second provider deactivates the
// first; exactly one row is ever active.
func TestActiveProviderIsSingular(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.EnsureProvider(ctx, "a", "m1", "embeddings_a_m1", 8, true); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProvider(ctx, "b", "m2", "embeddings_b_m2", 8, true); err != nil {
		t.Fatal(err)
	}
	ps, err := s.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, p := range ps {
		if p.Active {
			active++
			if p.Name != "b" {
				t.Fatalf("active = %s, want b", p.Name)
			}
		}
	}
	if active != 1 {
		t.Fatalf("active rows = %d, want 1", active)
	}
}

func TestUpsertEmbeddings(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.EnsureProvider(ctx, "a", "m", "embeddings_a_m", 3, true); err != nil {
		t.Fatal(err)
	}
	n := testNote("notes/vec.md")
	if err := s.UpsertNote(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceChunks(ctx, n.Path, []Chunk{{Ordinal: 0, Content: "c", ContentHash: "h"}}); err != nil {
		t.Fatal(err)
	}
	var chunkID uuid.UUID
	if err := s.pool.QueryRow(ctx, `select id from chunks where note_path = $1`, n.Path).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}

	vecs := map[uuid.UUID][]float32{chunkID: {1, 0, 0}}
	if err := s.UpsertEmbeddings(ctx, "embeddings_a_m", vecs); err != nil {
		t.Fatal(err)
	}
	// Upsert twice — conflict path.
	vecs[chunkID] = []float32{0, 1, 0}
	if err := s.UpsertEmbeddings(ctx, "embeddings_a_m", vecs); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `select count(*) from embeddings_a_m`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("embedding rows = %d, want 1", count)
	}

	// Cascade: deleting the note removes chunk and embedding.
	if err := s.DeleteNote(ctx, n.Path); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `select count(*) from embeddings_a_m`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("embedding rows after cascade = %d, want 0", count)
	}
}

func TestBadTableNameRejected(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.EnsureProvider(ctx, "x", "y", "drop table notes; --", 8, false); err == nil {
		t.Fatal("injection-shaped table name must be rejected")
	}
	if err := s.UpsertEmbeddings(ctx, "not_an_embeddings_table", map[uuid.UUID][]float32{}); err == nil {
		t.Fatal("non-conforming table name must be rejected")
	}
	_ = context.Background()
}
