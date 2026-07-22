package store

import (
	"strings"
	"testing"
)

// The boot-time derived-schema probe passes on a freshly migrated schema and
// fails loudly -- naming the column and the rebuild -- when notes.fts is missing,
// the state an already-migrated database lands in on upgrade (the column was
// folded into an applied migration and is never re-applied).
func TestVerifyDerivedSchema(t *testing.T) {
	s, ctx := testStore(t)

	if err := s.VerifyDerivedSchema(ctx); err != nil {
		t.Fatalf("freshly migrated schema failed verification: %v", err)
	}

	// Simulate the upgrade gap: an existing database at version 1 without the
	// folded column. Dropping the column takes its GIN index with it.
	if _, err := s.pool.Exec(ctx, "alter table notes drop column fts"); err != nil {
		t.Fatalf("drop fts column: %v", err)
	}

	err := s.VerifyDerivedSchema(ctx)
	if err == nil {
		t.Fatal("verification passed with notes.fts missing")
	}
	if !strings.Contains(err.Error(), "notes.fts") || !strings.Contains(err.Error(), "drop schema public cascade") {
		t.Errorf("error should name the column and the rebuild command, got: %v", err)
	}
}
