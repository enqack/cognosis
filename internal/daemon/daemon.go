// Package daemon owns the process lifecycle: a linear fatal-error startup
// sequence, the single-instance lock, self-daemonization, and graceful
// shutdown. It fails loudly and completely -- no degraded mode where some
// tools work and others don't: a panic in a primary runner (the MCP server or
// the file watcher) still brings the whole daemon down. The one nuance is
// per-item background work -- a single reconcile-sweep file, a fire-and-forget
// lazy-migration batch -- where a panic is recovered and logged (RecoverPanic)
// so one bad item can't crash the process; the tool surface stays all-or-nothing.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
)

// Reconciler is the boot-time vault reconciliation step; the watch package
// implements it. It runs before MCP connections are accepted.
type Reconciler interface {
	Reconcile(ctx context.Context, s *store.Store) error
}

// Runner is a long-lived component started after the lock is held (the file
// watcher and the MCP server). It must respect ctx cancellation.
type Runner interface {
	Run(ctx context.Context) error
}

// Options carries the pluggable steps so phases can land incrementally.
type Options struct {
	Reconciler Reconciler
	Runners    []Runner
	// Embedder is the active embedding provider. Its Health check is a fatal
	// startup gate, and its table is provisioned before serving. nil skips
	// both (reported as skipped by status).
	Embedder embed.Provider
	// MakeRunners builds runners that need the connected store (the MCP
	// server); called after provisioning, before the lock. A construction
	// error (e.g. a non-loopback bind address) is fatal -- the daemon refuses
	// to start rather than serving partially.
	MakeRunners func(s *store.Store) ([]Runner, error)
}

// Run executes the startup order and then blocks until ctx is cancelled or a
// runner fails. Order: Postgres reachability -> schema migrations -> boot
// reconciliation -> embedding check -> lock -> serve. Any failure in the first
// four steps is fatal; the daemon does not proceed in a partial state.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) error {
	log = log.With("component", "daemon")

	// 1. Postgres reachability (named target on failure).
	dsn, err := store.ResolveDSN(cfg.DSN)
	if err != nil {
		return fmt.Errorf("startup: postgres: %w", err)
	}
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("startup: postgres: %w", err)
	}
	defer s.Close()
	log.Info("postgres reachable")

	// 2. Schema migrations, auto-applied before serving.
	if err := store.Migrate(dsn); err != nil {
		return fmt.Errorf("startup: schema migration: %w", err)
	}
	log.Info("schema current")

	// History repo exists before reconciliation so drift commits have
	// somewhere to land.
	if err := cfg.Paths().EnsureDirs(); err != nil {
		return fmt.Errorf("startup: directories: %w", err)
	}
	hist := vault.NewHistory(cfg.KBPath)
	if err := hist.EnsureRepo(ctx); err != nil {
		return fmt.Errorf("startup: history repo: %w", err)
	}
	// Refresh the operator-facing history dashboard on boot so it reflects any
	// commits made while the daemon was down. Non-fatal -- it's a convenience.
	if err := hist.WriteDashboard(ctx); err != nil {
		log.Warn("history dashboard refresh failed", "err", err)
	}

	// 3. Single-instance lock -- acquired before any reconciliation so the boot
	// integrity scan can never race a second daemon's writes. Two guards:
	//   - the local PID lockfile: fast same-machine double-start refusal;
	//   - the Postgres session advisory lock: the cross-machine arbiter -- two
	//     daemons on different hosts pointing at one database cannot both run,
	//     which the lockfile alone (bound to a local filesystem) cannot prevent.
	lock, err := AcquireLock(cfg.Paths().LockFile())
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer func() { _ = lock.Release() }()
	releaseInstance, instanceAlive, err := s.AcquireInstanceLock(ctx)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer releaseInstance()
	log.Info("single-instance lock held", "pid", os.Getpid(), "lock", cfg.Paths().LockFile())

	// 4. Embedding provider reachability -- fatal, not degraded -- then table
	// provisioning (probe/known dimension -> create table + index -> register
	// as active).
	//
	// This must precede reconciliation. Indexing a note embeds its chunks, so
	// reconciliation needs an active provider; against a fresh schema there
	// isn't one yet, and every note failed with "no active embedding provider
	// registered" -- the vault only indexed on a *second* boot, once the
	// provider row from the first boot existed. Ordinary restarts hid it
	// because the row persists. Failing the health check before touching the
	// vault is also the better failure: no half-reconciled index.
	if opts.Embedder != nil {
		if err := opts.Embedder.Health(ctx); err != nil {
			return fmt.Errorf("startup: embedding provider: %w", err)
		}
		dim, err := opts.Embedder.Dimension(ctx)
		if err != nil {
			return fmt.Errorf("startup: embedding provider dimension: %w", err)
		}
		table := embed.TableSlug(opts.Embedder.Name(), opts.Embedder.Model())
		if err := s.EnsureProvider(ctx, opts.Embedder.Name(), opts.Embedder.Model(), table, dim, true); err != nil {
			return fmt.Errorf("startup: embedding provisioning: %w", err)
		}
		log.Info("embedding provider ready", "provider", opts.Embedder.Name(),
			"model", opts.Embedder.Model(), "dim", dim, "table", table)
	} else {
		log.Info("embedding provider not configured; skipped")
	}

	// 5. Boot-time vault reconciliation.
	if opts.Reconciler != nil {
		if err := opts.Reconciler.Reconcile(ctx, s); err != nil {
			return fmt.Errorf("startup: vault reconciliation: %w", err)
		}
		log.Info("vault reconciled")
	}

	// 6. Serve: run components until cancellation.
	log.Info("daemon up", "pid", os.Getpid())
	runners := opts.Runners
	if opts.MakeRunners != nil {
		made, err := opts.MakeRunners(s)
		if err != nil {
			return fmt.Errorf("startup: %w", err)
		}
		runners = append(runners, made...)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Liveness guard: if the pinned instance-lock connection dies, the advisory
	// lock is silently released and a second daemon could boot. Detect that and
	// shut down rather than run unguarded.
	go watchInstanceLock(runCtx, cancel, instanceAlive, log)

	errCh := make(chan error, len(runners))
	for _, r := range runners {
		go func(r Runner) {
			var runErr error
			func() {
				defer RecoverPanic(log, fmt.Sprintf("%T.Run", r), func(err error) { runErr = err })
				runErr = r.Run(runCtx)
			}()
			errCh <- runErr
		}(r)
	}

	// Shutdown must drain the runners before returning: the deferred lock
	// releases fire on return, and a watcher mid-write still owns the vault in
	// every sense that matters. Returning without waiting released the
	// single-instance lock while an index commit was in flight -- observed as a
	// dirty vault tree failing PurgePath's filter-branch in the daemon check.
	// The watcher shields each started unit of work from cancellation
	// (watch.graceful) and honours it between units, so cancel + drain lets an
	// in-flight write actually finish -- cancellation alone would propagate
	// into the write and kill the embed call mid-flight. The drain timeout
	// bounds a runner whose shielded work hangs anyway; the watcher's own
	// grace bound sits below it.
	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", ctx.Err())
		cancel()
		drainRunners(errCh, len(runners), log)
		return nil
	case err := <-errCh:
		cancel()
		drainRunners(errCh, len(runners)-1, log)
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("component failed: %w", err)
		}
		return nil
	}
}

// runnerDrainTimeout bounds how long shutdown waits for runners to observe
// cancellation. Generous because an in-flight index includes an embedding
// HTTP call; bounded because a hung runner must not wedge shutdown forever --
// past it, locks release anyway and the warning names the trade.
const runnerDrainTimeout = 15 * time.Second

func drainRunners(errCh <-chan error, n int, log *slog.Logger) {
	deadline := time.After(runnerDrainTimeout)
	for range n {
		select {
		case <-errCh:
		case <-deadline:
			log.Warn("shutdown: runners did not stop within the drain timeout; releasing locks anyway")
			return
		}
	}
}

// instanceLockPingInterval is how often the liveness guard probes the pinned
// instance-lock connection. Short enough to notice a lost lock quickly, far
// too infrequent to thrash Postgres (the heartbeat-I/O concern that ruled out a
// lease table).
const instanceLockPingInterval = 5 * time.Second

// watchInstanceLock stops the daemon if the single-instance advisory lock's
// connection is lost -- a dropped connection releases the lock server-side, so
// continuing would abandon the invariant.
func watchInstanceLock(ctx context.Context, stop context.CancelFunc, alive func(context.Context) error, log *slog.Logger) {
	ticker := time.NewTicker(instanceLockPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := alive(ctx); err != nil {
				if ctx.Err() != nil {
					return // shutting down anyway
				}
				log.Error("instance-lock connection lost; shutting down to preserve the single-instance invariant", "err", err)
				stop()
				return
			}
		}
	}
}

// SignalContext returns a context cancelled on SIGTERM/SIGINT -- the graceful
// shutdown path (context cancellation propagates to every runner).
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
}
