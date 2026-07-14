package store

import (
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

// SchemaStatus reports the applied version, dirtiness, and pending count —
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
