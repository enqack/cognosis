package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// mint creates a token row under a name, returning its id. The hash is opaque
// here — these tests are about the name/lifecycle rules, not verification.
func mint(t *testing.T, s *store.Store, name string) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(t.Context(), id, name, "argon2id$stub"); err != nil {
		t.Fatalf("CreateToken(%q): %v", name, err)
	}
	return id
}

// TestTokenNameReusableAfterRevocation — uniqueness is scoped to live tokens,
// so rotation is revoke-then-recreate under the same name rather than burning
// it. Against the old global UNIQUE the first half fails with Conflict.
func TestTokenNameReusableAfterRevocation(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()

	mint(t, s, "desktop")
	if err := s.RevokeToken(ctx, "desktop"); err != nil {
		t.Fatal(err)
	}
	mint(t, s, "desktop") // the point of the change

	// The other half, and the one that actually guards the schema: a second
	// LIVE row under the same name must still be refused. Without this, a
	// schema that dropped the constraint and failed to install the partial
	// index would pass the assertion above and silently allow duplicates.
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	err = s.CreateToken(ctx, id, "desktop", "argon2id$stub")
	if err == nil {
		t.Fatal("two live tokens share a name: the live-name unique index is missing")
	}
	if !cogerr.Is(err, cogerr.Conflict) {
		t.Fatalf("kind = %v, want Conflict", err)
	}
}

// TestRevokeTokenTargetsTheLiveRow — `where revoked_at is null` becomes
// load-bearing once several rows can share a name: revoking must hit the
// current credential, not an already-dead one.
func TestRevokeTokenTargetsTheLiveRow(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()

	old := mint(t, s, "desktop")
	if err := s.RevokeToken(ctx, "desktop"); err != nil {
		t.Fatal(err)
	}
	current := mint(t, s, "desktop")
	if err := s.RevokeToken(ctx, "desktop"); err != nil {
		t.Fatal(err)
	}

	tokens, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var oldAt, currentAt string
	for _, tk := range tokens {
		if tk.RevokedAt == nil {
			t.Fatalf("token %q still live after revoke", tk.Name)
		}
		switch tk.ID {
		case old:
			oldAt = tk.RevokedAt.String()
		case current:
			currentAt = tk.RevokedAt.String()
		}
	}
	if oldAt == "" || currentAt == "" {
		t.Fatalf("expected both rows revoked, got old=%q current=%q", oldAt, currentAt)
	}
	if oldAt == currentAt {
		t.Fatal("both rows carry the same revoked_at: the second revoke re-stamped " +
			"the already-revoked row instead of the live one")
	}

	// Nothing live left to revoke.
	if err := s.RevokeToken(ctx, "desktop"); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("third revoke = %v, want NotFound", err)
	}
}

// TestPruneRevokedTokensKeepsReferenced — prune must never orphan the audit
// trail. audit_log joins to tokens at read time, so a referenced row stays even
// though it is revoked; the FK (NO ACTION) is the backstop behind the predicate.
func TestPruneRevokedTokensKeepsReferenced(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()

	used := mint(t, s, "used")
	unused := mint(t, s, "unused")
	live := mint(t, s, "live")
	_ = live // never revoked; must survive regardless of references

	if err := s.AppendAudit(ctx, &used, "query_knowledge", "", "text_len=4", true, ""); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"used", "unused"} {
		if err := s.RevokeToken(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	// The dry run must predict exactly what the delete does — a preview that can
	// drift from its action is worse than no preview.
	preview, err := s.PrunableTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.PruneRevokedTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || preview[0] != "unused" {
		t.Fatalf("PrunableTokens = %v, want [unused]", preview)
	}
	if len(deleted) != 1 || deleted[0] != "unused" {
		t.Fatalf("PruneRevokedTokens = %v, want [unused]", deleted)
	}

	remaining := map[string]bool{}
	tokens, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tokens {
		remaining[tk.Name] = true
	}
	if !remaining["used"] {
		t.Fatal("pruned a token referenced by audit_log; the join now dangles")
	}
	if !remaining["live"] {
		t.Fatal("pruned a live token")
	}
	if remaining["unused"] {
		t.Fatal("unreferenced revoked token survived the prune")
	}
	_ = unused
}
