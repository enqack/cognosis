package cli

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// TestProbeDaemonReadsTheAdvisoryLock -- the probe must reflect who holds
// LockInstance, not what the local PID file says. Advisory locks are
// database-wide rather than schema-scoped, which is exactly the property that
// lets this answer the question across hosts -- and exactly why this test
// takes a private database (NewDB): on the shared test database, concurrent
// packages that boot daemons hold the same lock and turn the lock-free half
// into a coin flip.
func TestProbeDaemonReadsTheAdvisoryLock(t *testing.T) {
	s, dsn := storetest.NewDB(t)
	ctx := context.Background()
	cfg := &config.Config{DSN: dsn}

	release, err := s.AcquireAdvisory(ctx, store.LockInstance)
	if err != nil {
		t.Fatalf("instance lock held on a private database; nothing else can reach it: %v", err)
	}

	// Held by this test, standing in for a daemon anywhere.
	if got := probeDaemon(ctx, cfg); got != daemonPresent {
		t.Errorf("probe = %v with the instance lock held, want daemonPresent", got)
	}

	release()
	if got := probeDaemon(ctx, cfg); got != daemonAbsent {
		t.Errorf("probe = %v with the lock free, want daemonAbsent", got)
	}
}

// An unreachable database is not evidence that no daemon is running. Callers
// branch on this, and conflating it with daemonAbsent would license a direct
// write in exactly the case the probe could not rule one out.
func TestProbeDaemonUnknownWhenPostgresUnreachable(t *testing.T) {
	cfg := &config.Config{DSN: "postgres://127.0.0.1:1/nonexistent?connect_timeout=1"}
	if got := probeDaemon(context.Background(), cfg); got != daemonUnknown {
		t.Errorf("probe = %v against an unreachable database, want daemonUnknown", got)
	}
}
