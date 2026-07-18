// Package store owns the Postgres side of the derived index: connection,
// versioned schema migrations, and CRUD over notes/chunks/links.
// Raw pgx errors never leave this package (cogerr contract).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
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

// HNSWEfSearch is the per-session hnsw.ef_search applied by Connect. It must
// stay >= the retrieval candidate pool (query.candidatePool): pgvector's
// default is 40, and an HNSW scan without iterative_scan returns at most
// ef_search rows, so a pool of 50 silently came back short. Asserted against
// the pool by TestCandidatePoolWithinScanCapacity.
const HNSWEfSearch = 100

// HNSWIterativeScan keeps the scan going until the LIMIT is satisfied instead
// of stopping after one ef_search-sized candidate list. This is the setting
// that actually fixes filtered retrieval: with a project or status predicate,
// a fixed candidate list is filtered *after* the graph walk, so a scope holding
// a quarter of the corpus returned as few as 8 rows for a requested 50 — recall
// 0.205 against exact KNN. Raising ef_search alone does not fix it (0.883 at
// ef_search=200); it only enlarges the list being filtered down.
//
// relaxed_order over strict_order deliberately. relaxed admits slight
// out-of-distance-order results (Kendall ~0.995 vs 1.000) but retrieves more
// of the true neighbours (0.981 vs 0.969 recall on the worst scope), and RRF
// consumes rank position with k=60 damping — rank 1 scores 1/61 and rank 50
// scores 1/110, so the whole rank range spans 1.8x while a *missing* item
// contributes exactly 0. Recall dominates ordering under this fusion.
const HNSWIterativeScan = "relaxed_order"

// scanSettings are applied to every pooled connection. Values are constants,
// never caller input.
//
// Both settings go in ONE statement deliberately. AfterConnect is on the
// critical path of every new pooled connection, and two round trips there was
// measurable: it widened the window between boot reconciliation indexing a
// note and committing it to vault history enough to lose a race the
// daemon check had been winning.
var scanSettings = []string{
	fmt.Sprintf("set hnsw.ef_search = %d; set hnsw.iterative_scan = '%s'",
		HNSWEfSearch, HNSWIterativeScan),
}

// Connect opens a pool with pgvector types registered on every connection and
// the scan settings above applied.
func Connect(ctx context.Context, dsn string) (*Store, error) {
	return connect(ctx, dsn, scanSettings)
}

// ConnectWithScanSettings is Connect with the per-connection scan settings
// replaced. It exists solely for internal/query/retrievaleval, which needs a
// pool reproducing pgvector's pre-fix defaults to measure the fix against.
//
// Passing the settings through the DSN's startup-packet options does not work
// for this: AfterConnect runs afterwards and would overwrite them, which
// silently turned the harness's pre-fix baseline into a second copy of the
// fixed engine and made the comparison report no change at all.
func ConnectWithScanSettings(ctx context.Context, dsn string, settings []string) (*Store, error) {
	return connect(ctx, dsn, settings)
}

func connect(ctx context.Context, dsn string, settings []string) (*Store, error) {
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
		// Scan settings first, and independent of the type registration
		// below: Postgres accepts dotted (extension-namespaced) GUC names as
		// placeholders even before the extension loads, so these apply on
		// pre-migration connections too — which matters because such a
		// connection is pooled and reused long after migrations finish.
		//
		// A failure here is not fatal. On a pgvector too old to know
		// iterative_scan, retrieval still works; it just under-retrieves on
		// filtered scopes exactly as it did before this setting existed.
		// Breaking every connection would be the worse outcome.
		for _, s := range settings {
			if _, err := conn.Exec(ctx, s); err != nil {
				slog.Warn("could not apply pgvector scan setting; "+
					"filtered retrieval may under-return (needs pgvector >= 0.8)",
					"setting", s, "reason", err)
			}
		}
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
