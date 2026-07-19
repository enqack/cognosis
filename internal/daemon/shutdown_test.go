package daemon_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/daemon"
)

// slowStopRunner models the watcher finishing an in-flight write after
// cancellation: it observes ctx.Done, keeps "writing" for a moment, and only
// then records that it stopped cleanly.
type slowStopRunner struct {
	stopped atomic.Bool
}

func (r *slowStopRunner) Run(ctx context.Context) error {
	<-ctx.Done()
	time.Sleep(500 * time.Millisecond)
	r.stopped.Store(true)
	return nil
}

// TestShutdownDrainsRunnersBeforeReturn pins the shutdown order: Run must not
// return — and therefore must not release the instance lock, which its defers
// do on return — while a runner is still stopping. The pre-fix select returned
// on ctx.Done without waiting, which released the vault's ownership with a
// watcher write still in flight; daemon.sh caught it as a dirty vault tree
// failing PurgePath. Verified to fail against that defect.
func TestShutdownDrainsRunnersBeforeReturn(t *testing.T) {
	dsn := os.Getenv("COGNOSIS_TEST_DSN")
	if dsn == "" {
		t.Skip("COGNOSIS_TEST_DSN not set; integration tests need a real Postgres (run pg-start in the dev shell)")
	}
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("COGNOSIS_DSN", dsn)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	// The instance lock is database-global and other test packages (the store's
	// mutual-exclusion test) hold it transiently on the same test database, so
	// a boot-time Conflict here is contention, not failure — retry briefly.
	r := &slowStopRunner{}
	var runErr error
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// Let startup finish, then request shutdown.
			time.Sleep(300 * time.Millisecond)
			cancel()
		}()
		runErr = daemon.Run(ctx, cfg, slog.New(slog.DiscardHandler), daemon.Options{
			Runners: []daemon.Runner{r},
		})
		cancel()
		if runErr == nil || !strings.Contains(runErr.Error(), "another cognosis daemon") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if runErr != nil {
		t.Fatalf("daemon.Run: %v", runErr)
	}
	if !r.stopped.Load() {
		t.Fatal("Run returned before its runner finished stopping — locks were released with work still in flight")
	}
}
