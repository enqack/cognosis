// Package storetest provides the shared integration-test harness: a real
// Postgres (via COGNOSIS_TEST_DSN, provided by the dev shell) with a per-run
// isolated schema, migrated and dropped per test.
package storetest

import (
	"context"
	"fmt"
	"math/rand"
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
	base := os.Getenv("COGNOSIS_TEST_DSN")
	if base == "" {
		t.Skip("COGNOSIS_TEST_DSN not set; integration tests need a real Postgres (run pg-start in the dev shell)")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("cog_test_%d_%d", os.Getpid(), rand.Int63())

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
