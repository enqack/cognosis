package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Boot-time lock file: single active daemon instance is an invariant.
// The file holds the running PID; a second start detects it and refuses.

type Lock struct {
	path string
}

// AcquireLock takes the daemon lock or fails with Conflict if another live
// process holds it. A lock left by a dead process (stale PID) is reclaimed.
func AcquireLock(path string) (*Lock, error) {
	const op = "daemon.AcquireLock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if _, werr := fmt.Fprintf(f, "%d\n", os.Getpid()); werr != nil {
				_ = f.Close()
				return nil, cogerr.E(op, cogerr.Internal, werr)
			}
			if cerr := f.Close(); cerr != nil {
				return nil, cogerr.E(op, cogerr.Internal, cerr)
			}
			return &Lock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		pid, perr := ReadLockPID(path)
		if perr == nil && processAlive(pid) {
			return nil, cogerr.Ef(op, cogerr.Conflict,
				"another cognosis daemon is running (pid %d, lock %s)", pid, path)
		}
		// Stale lock: holder is gone — reclaim and retry once.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, cogerr.E(op, cogerr.Internal, rmErr)
		}
	}
	return nil, cogerr.Ef(op, cogerr.Conflict, "could not acquire lock %s", path)
}

func (l *Lock) Release() error {
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return cogerr.E("daemon.Lock.Release", cogerr.Internal, err)
	}
	return nil
}

// ReadLockPID returns the PID recorded in the lock file.
func ReadLockPID(path string) (int, error) {
	const op = "daemon.ReadLockPID"
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, cogerr.E(op, cogerr.NotFound, err)
		}
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, cogerr.Ef(op, cogerr.Validation, "lock file %s does not hold a pid: %q", path, b)
	}
	return pid, nil
}

// processAlive reports whether a PID refers to a live process (signal 0).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
