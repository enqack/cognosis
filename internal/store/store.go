// Package store owns the Postgres side of the derived index: connection,
// versioned schema migrations, and CRUD over notes/chunks/links.
// Raw pgx errors never leave this package (cogerr contract).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Store wraps a pgx pool; all query methods hang off it.
type Store struct {
	pool *pgxpool.Pool
}

// ResolveDSN picks the connection string: an explicit configured DSN wins
// (config already layers COGNOSIS_DSN env over file); otherwise self-locate a
// dev Postgres by walking up from cwd to a .pg-data dir (the flake's pg-start
// layout, port 5434).
func ResolveDSN(configured string) (string, error) {
	const op = "store.ResolveDSN"
	if configured != "" {
		return configured, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", cogerr.E(op, cogerr.Internal, err)
	}
	for {
		pgdata := filepath.Join(dir, ".pg-data")
		if st, err := os.Stat(pgdata); err == nil && st.IsDir() {
			sock := sockDir(dir, pgdata)
			return "postgres:///cognosis?host=" + url.QueryEscape(sock) + "&port=5434", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", cogerr.Ef(op, cogerr.Unavailable,
				"dsn not configured and no .pg-data found above cwd — set dsn in config or run pg-start")
		}
		dir = parent
	}
}

// sockDir mirrors the flake's macOS sun_path-length fallback: when .pg-data's
// path is too long for a Unix socket, the flake persists the real socket dir
// to <repo>/.pg-socket-path; recompute its hashed-$TMPDIR choice if missing.
func sockDir(repoDir, pgdata string) string {
	if len(pgdata) <= 90 {
		return pgdata
	}
	if b, err := os.ReadFile(filepath.Join(repoDir, ".pg-socket-path")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	sum := sha256.Sum256([]byte(repoDir))
	return filepath.Join(tmp, "cognosis-"+hex.EncodeToString(sum[:])[:12])
}

// Connect opens a pool with pgvector types registered on every connection.
func Connect(ctx context.Context, dsn string) (*Store, error) {
	const op = "store.Connect"
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Validation, err)
	}
	// On a fresh database the vector extension doesn't exist until migration
	// 0001 creates it — and the startup reachability check connects before
	// migrations run. Missing type is fine then: nothing can query embeddings
	// before provisioning; connections opened after the migration register the
	// types normally.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
			if strings.Contains(err.Error(), "vector type not found") {
				return nil
			}
			return err
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Unavailable, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, cogerr.E(op, cogerr.Unavailable,
			fmt.Errorf("postgres unreachable at %s: %w", redactDSN(dsn), err))
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// redactDSN strips credentials for error messages — startup failures should
// name the attempted target, not its secrets.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable dsn>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.Redacted()
}
