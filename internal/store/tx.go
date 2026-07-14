package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/enqack/cognosis/internal/cogerr"
)

// pgxTx is the slice of pgx.Tx the shared transactional helpers need — lets
// the same helper run inside IndexNote's transaction or a standalone one.
type pgxTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Advisory lock keys for whole-KB operations. Session-scoped Postgres
// advisory locks: a concurrent second caller gets an explicit
// already-in-progress error instead of racing shared state.
const (
	// LockInstance guards the single-daemon-per-database invariant. Unlike the
	// operation locks below it is held for the daemon's entire lifetime on a
	// dedicated connection (see AcquireInstanceLock), so it is the cross-machine
	// arbiter the local PID lockfile can never be.
	LockInstance int64 = 0xC0600 // single active daemon per database
	LockCompile  int64 = 0xC0601 // compile_lifecycle
	LockMigrate  int64 = 0xC0602 // embedding migration
)

// AcquireInstanceLock takes the process-lifetime single-instance lock. It runs
// on a dedicated connection opened straight from the pool's config — session
// advisory locks live on their backend connection, and a pooled conn would be
// recycled back into circulation, silently dropping the lock. It fails with
// Conflict when another daemon (even on another machine) already owns this
// database. Because the lock releases the instant the connection dies, a
// crashed daemon never wedges the invariant — no stale-lease window to tune.
//
// release drops the lock and closes the connection. alive pings the pinned
// connection so the caller can detect a silently-lost lock and stop rather
// than let a second instance boot behind its back.
func (s *Store) AcquireInstanceLock(ctx context.Context) (release func(), alive func(context.Context) error, err error) {
	const op = "store.AcquireInstanceLock"
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
	if err != nil {
		return nil, nil, cogerr.E(op, cogerr.Unavailable, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, LockInstance).Scan(&got); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, nil, cogerr.E(op, cogerr.Internal, err)
	}
	if !got {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, nil, cogerr.Ef(op, cogerr.Conflict,
			"another cognosis daemon already owns this database")
	}
	release = func() {
		// Best-effort unlock; closing the session drops the lock regardless.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, LockInstance)
		_ = conn.Close(context.WithoutCancel(ctx))
	}
	alive = func(ctx context.Context) error { return conn.Ping(ctx) }
	return release, alive, nil
}

// AcquireAdvisory takes a whole-KB advisory lock, or fails with Conflict when
// another session holds it. The returned release func must be called; it also
// returns the dedicated connection to the pool.
func (s *Store) AcquireAdvisory(ctx context.Context, key int64) (release func(), err error) {
	const op = "store.AcquireAdvisory"
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Unavailable, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	if !got {
		conn.Release()
		return nil, cogerr.Ef(op, cogerr.Conflict, "operation already in progress (advisory lock %#x held)", key)
	}
	return func() {
		// Best-effort unlock; releasing the session drops the lock regardless.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock($1)`, key)
		conn.Release()
	}, nil
}
