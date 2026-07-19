package daemon

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/enqack/cognosis/internal/config"
)

// TestDaemonizeReturnsTheChildPID -- `cognosis start` prints the returned pid,
// and it printed -1. Cause: Release marks a spent handle by setting
// Process.Pid to -1, and the old
//
//	return cmd.Process.Pid, cmd.Process.Release()
//
// left the read racing the call. Go specifies left-to-right ordering for
// function calls in a return statement but *not* for a plain operand beside
// one, so the field could be read after Release had already zeroed it.
//
// The child here is the test binary re-invoked with daemon flags it does not
// understand; it exits immediately. That is fine -- this asserts on the pid the
// parent reports, not on anything the child does.
func TestDaemonizeReturnsTheChildPID(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{
		ConfigDir: filepath.Join(dir, "config"),
		DataDir:   filepath.Join(dir, "data"),
		StateDir:  filepath.Join(dir, "state"),
		CacheDir:  filepath.Join(dir, "cache"),
	}

	pid, err := Daemonize(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 {
		t.Fatalf("Daemonize returned pid %d; `cognosis start` prints this verbatim", pid)
	}
	if pid == os.Getpid() {
		t.Fatalf("Daemonize returned the parent's own pid %d", pid)
	}
	// Reap it if it is somehow still around -- it was setsid'd and released, so
	// a signal is the only handle left and failure is not interesting.
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
