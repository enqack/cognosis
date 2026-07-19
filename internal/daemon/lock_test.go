package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
)

func TestLockLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")

	l, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := ReadLockPID(path)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("lock pid = %d (%v), want %d", pid, err, os.Getpid())
	}

	// Second acquire while held (by this live process) must refuse.
	if _, err := AcquireLock(path); !cogerr.Is(err, cogerr.Conflict) {
		t.Fatalf("second acquire = %v, want Conflict", err)
	}

	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	_ = l2.Release()
}

func TestStaleLockReclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	// A PID that cannot be alive: max pid on darwin/linux is far below this.
	if err := os.WriteFile(path, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("stale lock not reclaimed: %v", err)
	}
	_ = l.Release()
}

func TestCorruptLockNotReclaimedSilently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unreadable PID -> treated as stale (holder unverifiable) and reclaimed;
	// what matters is we don't crash and the lock ends up held by us.
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("corrupt lock: %v", err)
	}
	defer func() { _ = l.Release() }()
	pid, err := ReadLockPID(path)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("lock not rewritten with our pid: %d, %v", pid, err)
	}
}
