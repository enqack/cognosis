// Package storetest provides the shared integration-test harness: a real
// Postgres (via COGNOSIS_TEST_DSN, provided by the dev shell) with a per-run
// isolated schema, migrated and dropped per test.
package storetest

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/enqack/cognosis/internal/store"
)

// New returns a migrated Store in its own schema, plus the DSN scoped to that
// schema. Skips the test when COGNOSIS_TEST_DSN is unset.
func New(t *testing.T) (*store.Store, string) {
	t.Helper()
	return NewTB(t)
}

// NewDB returns a migrated Store in its own throwaway *database*, plus the DSN
// scoped to it. Schema isolation (New) is cheaper and right for almost every
// test, but advisory locks are database-scoped, not schema-scoped -- so a test
// that asserts on who holds LockInstance shares that lock with every concurrent
// test in every package running against COGNOSIS_TEST_DSN. Tests that need the
// advisory keyspace to themselves take a private database instead.
func NewDB(t *testing.T) (*store.Store, string) {
	t.Helper()
	base := os.Getenv("COGNOSIS_TEST_DSN")
	if base == "" {
		t.Skip("COGNOSIS_TEST_DSN not set; integration tests need a real Postgres (run pg-start in the dev shell)")
	}
	ctx := context.Background()
	var suffix [8]byte
	if _, err := crand.Read(suffix[:]); err != nil {
		t.Fatalf("random database suffix: %v", err)
	}
	name := fmt.Sprintf("cog_testdb_%d_%d", os.Getpid(), binary.BigEndian.Uint64(suffix[:]))

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`create database %q`, name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		// FORCE terminates any connection a probe or CLI under test left behind;
		// without it a straggler turns the drop into a flake of its own.
		_, _ = admin.Exec(ctx, fmt.Sprintf(`drop database %q with (force)`, name))
		_ = admin.Close(ctx)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()

	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s, dsn
}

// NewTB is New over testing.TB, so benchmarks can build a schema too. New
// remains the spelling for ordinary tests.
func NewTB(t testing.TB) (*store.Store, string) {
	t.Helper()
	base := os.Getenv("COGNOSIS_TEST_DSN")
	if base == "" {
		t.Skip("COGNOSIS_TEST_DSN not set; integration tests need a real Postgres (run pg-start in the dev shell)")
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

	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s, dsn
}
