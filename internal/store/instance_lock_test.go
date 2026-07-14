package store

import (
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
)

// TestInstanceLockMutualExclusion proves the single-instance advisory lock is a
// real cross-connection mutex: a second acquire is refused while the first is
// held, and releasing frees it for the next holder.
func TestInstanceLockMutualExclusion(t *testing.T) {
	s, ctx := testStore(t)

	release1, alive1, err := s.AcquireInstanceLock(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := alive1(ctx); err != nil {
		t.Fatalf("holder connection should be alive: %v", err)
	}

	// A second acquire on a distinct connection is refused with Conflict while
	// the first still holds the lock.
	if _, _, err := s.AcquireInstanceLock(ctx); err == nil {
		release1()
		t.Fatal("second acquire should fail while the lock is held")
	} else if !cogerr.Is(err, cogerr.Conflict) {
		release1()
		t.Fatalf("want Conflict, got %v", err)
	}

	// Releasing frees the lock for the next holder.
	release1()
	release2, _, err := s.AcquireInstanceLock(ctx)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}
