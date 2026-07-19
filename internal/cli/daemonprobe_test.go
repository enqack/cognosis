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
// lets this answer the question across hosts.
//
// A live daemon is a fixture here rather than a reason to skip: if it already
// holds the lock, that is the exact condition the probe exists to detect, so
// assert on it and skip only the half that needs the lock free. The naive
// version skipped outright and threw away the more realistic case.
func TestProbeDaemonReadsTheAdvisoryLock(t *testing.T) {
	s, dsn := storetest.New(t)
	ctx := context.Background()
	cfg := &config.Config{DSN: dsn}

	release, err := s.AcquireAdvisory(ctx, store.LockInstance)
	if err != nil {
		// A real daemon owns it. That is the positive case, for free.
		if got := probeDaemon(ctx, cfg); got != daemonPresent {
			t.Fatalf("probe = %v while a real daemon holds the instance lock, want daemonPresent", got)
		}
		t.Skip("a daemon owns this database; the lock-free half needs it stopped")
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
