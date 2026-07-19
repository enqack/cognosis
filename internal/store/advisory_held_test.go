package store_test

import (
	"testing"
	"time"

	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// TestAdvisoryHeldObservesRealLocks -- AdvisoryHeld decodes an advisory key into
// the classid/objid pair pg_locks stores it under, and a wrong decode does not
// error: it returns false forever, which reads as "no daemon owns this
// database" and silently disables every guard built on it.
//
// So both directions are asserted against a lock genuinely held, rather than
// the false case alone -- which any broken query passes.
//
// The keys are per-run, not store.LockInstance/LockCompile/LockMigrate.
// Advisory locks are scoped to the *database*, while storetest isolates tests by
// *schema*, so every test in the run shares one advisory namespace: asserting
// that a production key is free races any concurrent test legitimately holding
// it. The decode under test does not depend on which key it is handed.
func TestAdvisoryHeldObservesRealLocks(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := t.Context()

	// Distinct, and unlikely to collide with a concurrent test's keys. Adjacent
	// values on purpose: they differ only in low bits, which is what a botched
	// mask would collapse.
	base := time.Now().UnixNano() & 0x7fffffff
	target, neighbour := base, base+1

	for _, key := range []int64{target, neighbour} {
		held, err := s.AdvisoryHeld(ctx, key)
		if err != nil {
			t.Fatalf("AdvisoryHeld(%#x): %v", key, err)
		}
		if held {
			t.Fatalf("key %#x already held before the test took it", key)
		}
	}

	release, err := s.AcquireAdvisory(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	held, err := s.AdvisoryHeld(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("AdvisoryHeld reported free while the lock was held; every ownership guard reads this as 'no daemon'")
	}

	held, err = s.AdvisoryHeld(ctx, neighbour)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Errorf("holding %#x made %#x look held; the key decode is collapsing distinct locks", target, neighbour)
	}

	release()

	held, err = s.AdvisoryHeld(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("AdvisoryHeld still reports the lock held after release")
	}
}

// The production keys must decode too -- they are the ones every guard actually
// passes. Only the "held" direction is safe to assert here, since another test
// may hold them concurrently; a wrong decode returns false for a held lock, so
// taking the lock first and requiring true still catches it.
func TestAdvisoryHeldDecodesProductionKeys(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := t.Context()

	for _, key := range []int64{store.LockInstance, store.LockCompile, store.LockMigrate} {
		release, err := s.AcquireAdvisory(ctx, key)
		if err != nil {
			// Held by a concurrent test: nothing to assert, and skipping beats
			// a flake.
			continue
		}
		held, err := s.AdvisoryHeld(ctx, key)
		release()
		if err != nil {
			t.Fatalf("AdvisoryHeld(%#x): %v", key, err)
		}
		if !held {
			t.Errorf("AdvisoryHeld(%#x) reported free while this test held it", key)
		}
	}
}
