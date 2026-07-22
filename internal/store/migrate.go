package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/migrations"
)

// VerifyDerivedSchema fails when the derived index predates a column the current
// code requires. Migrations are version-tracked, so a column folded into an
// already-applied migration -- notes.fts, added for the note-level keyword leg --
// is never applied on upgrade, and the query path would otherwise error once per
// request deep in the serve phase. Called at boot, right after Migrate, so a
// stale index is one fatal, actionable failure instead of a silent footgun.
func (s *Store) VerifyDerivedSchema(ctx context.Context) error {
	const op = "store.VerifyDerivedSchema"
	// Resolve `notes` through the connection's search_path (to_regclass) and check
	// that relation specifically -- a bare information_schema scan is schema-blind
	// and would match the column in any other schema, which the per-schema test
	// isolation makes visible.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`select exists (select 1 from pg_attribute
		   where attrelid = to_regclass('notes')
		     and attname = 'fts' and not attisdropped)`).Scan(&exists); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if !exists {
		return cogerr.Ef(op, cogerr.Validation,
			"derived index predates note-level full-text search: notes.fts is missing. This release "+
				"folded the column into the initial migration, so an already-migrated database will not "+
				"gain it on upgrade. Rebuild the derived index: run `drop schema public cascade; create "+
				"schema public;` on the database, then restart -- boot reconciliation re-indexes from the "+
				"vault (the markdown is untouched), and the tokens table is recreated so re-read "+
				"local-token afterward")
	}
	return nil
}

// Migrate applies pending schema migrations. Runs at daemon startup
// before MCP connections are accepted; idempotent when current.
func Migrate(dsn string) error {
	const op = "store.Migrate"
	m, db, err := newMigrator(dsn)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = db.Close() }()
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// SchemaStatus reports the applied version, dirtiness, and pending count --
// backs `cognosis schema status` and the daemon health check.
type SchemaStatus struct {
	Version uint
	Dirty   bool
	Latest  uint
	Pending int
}

func (s SchemaStatus) Current() bool { return !s.Dirty && s.Version == s.Latest }

// Status inspects schema currency without applying anything.
func Status(dsn string) (SchemaStatus, error) {
	const op = "store.Status"
	m, db, err := newMigrator(dsn)
	if err != nil {
		return SchemaStatus{}, cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = db.Close() }()
	defer func() { _, _ = m.Close() }()

	versions, err := embeddedVersions()
	if err != nil {
		return SchemaStatus{}, cogerr.E(op, cogerr.Internal, err)
	}
	st := SchemaStatus{Latest: versions[len(versions)-1]}

	v, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		st.Pending = len(versions)
	case err != nil:
		return SchemaStatus{}, cogerr.E(op, cogerr.Internal, err)
	default:
		st.Version, st.Dirty = v, dirty
		for _, ver := range versions {
			if ver > v {
				st.Pending++
			}
		}
	}
	return st, nil
}

func newMigrator(dsn string) (*migrate.Migrate, *sql.DB, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return m, db, nil
}

// embeddedVersions lists migration versions from the embedded FS, ascending.
func embeddedVersions() ([]uint, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, err
	}
	seen := map[uint]bool{}
	var out []uint
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		numStr, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(numStr, 10, 32)
		if err != nil {
			continue
		}
		if !seen[uint(n)] {
			seen[uint(n)] = true
			out = append(out, uint(n))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no embedded migrations found")
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
