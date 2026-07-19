package store_test

import (
	"testing"

	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// TestAdvisoryHeldObservesRealLocks — AdvisoryHeld decodes an advisory key into
// the classid/objid pair pg_locks stores it under, and a wrong decode does not
// error: it returns false forever, which reads as "no daemon owns this
// database" and silently disables every guard built on it.
//
// So both directions are asserted against a lock genuinely held on this
// connection, rather than the false case alone — which any broken query passes.
func TestAdvisoryHeldObservesRealLocks(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := t.Context()

	for _, key := range []int64{store.LockInstance, store.LockCompile, store.LockMigrate} {
		held, err := s.AdvisoryHeld(ctx, key)
		if err != nil {
			t.Fatalf("AdvisoryHeld(%#x): %v", key, err)
		}
		if held {
			t.Fatalf("lock %#x already held before the test took it", key)
		}
	}

	release, err := s.AcquireAdvisory(ctx, store.LockCompile)
	if err != nil {
		t.Fatal(err)
	}

	held, err := s.AdvisoryHeld(ctx, store.LockCompile)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("AdvisoryHeld reported free while the lock was held; every ownership guard reads this as 'no daemon'")
	}

	// A held lock must not make its neighbours look held. The keys differ only
	// in their low bits, which is exactly what a botched mask would collapse.
	for _, other := range []int64{store.LockInstance, store.LockMigrate} {
		held, err := s.AdvisoryHeld(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if held {
			t.Errorf("holding %#x made %#x look held; the key decode is collapsing distinct locks",
				store.LockCompile, other)
		}
	}

	release()

	held, err = s.AdvisoryHeld(ctx, store.LockCompile)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("AdvisoryHeld still reports the lock held after release")
	}
}
