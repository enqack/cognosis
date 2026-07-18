package query

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// This is the CI-tier guard for the scan-capacity fix. It is deliberately
// cheap — no corpus, no index, no timing, milliseconds — because the expensive
// sweeps that established *what* the settings should be live in
// internal/query/retrievaleval and are local-only. This one only guards that
// the invariant still holds.
//
// The regression it exists to catch: someone raises candidatePool past
// ef_search, or a refactor drops the AfterConnect hook, and the vector leg
// silently under-returns again with no symptom anywhere. That is exactly how
// the defect went unnoticed in the first place — a truncated candidate list
// produces plausible results, not an error.

func TestCandidatePoolWithinScanCapacity(t *testing.T) {
	if store.HNSWEfSearch < candidatePool {
		t.Fatalf("hnsw.ef_search (%d) < candidatePool (%d): without iterative_scan an HNSW "+
			"scan returns at most ef_search rows, so the vector leg cannot fill its pool",
			store.HNSWEfSearch, candidatePool)
	}
}

// The constants agreeing is necessary but not sufficient — they also have to
// reach the database. This asserts a connection opened by store.Connect really
// carries both settings.
func TestConnectAppliesScanSettings(t *testing.T) {
	ctx := context.Background()
	s, _ := storetest.New(t)

	efSearch, err := s.CurrentSetting(ctx, "hnsw.ef_search")
	if err != nil {
		t.Fatalf("read hnsw.ef_search: %v", err)
	}
	if efSearch != "100" {
		t.Errorf("hnsw.ef_search = %q on a pooled connection, want %q (store.HNSWEfSearch)",
			efSearch, "100")
	}

	iterative, err := s.CurrentSetting(ctx, "hnsw.iterative_scan")
	if err != nil {
		t.Fatalf("read hnsw.iterative_scan: %v", err)
	}
	if iterative != store.HNSWIterativeScan {
		t.Errorf("hnsw.iterative_scan = %q on a pooled connection, want %q",
			iterative, store.HNSWIterativeScan)
	}
}
