package query_test

import (
	"testing"

	"github.com/enqack/cognosis/internal/query"
)

// The accessor tests in tuning_test.go prove Tuning *resolves* correctly. They
// would all pass if Run ignored Tuning entirely. These prove it is actually
// wired into Run — a missed call site is otherwise invisible.

// DisableGraph must remove the graph leg. entries/garden.md is reachable only
// via the link graph (see TestGraphOnlyNoteAppears), so it is the sharpest
// available probe.
//
// This test is why DisableGraph exists: it first caught that GraphWeight=0 did
// NOT drop this note, because FuseRRF inserts a zero-weighted leg's items at
// score 0 rather than skipping them. "Weight 0" and "leg absent" are different
// configurations, and the truncation-masking experiment needs the latter.
func TestTuningDisableGraphDropsGraphOnlyNote(t *testing.T) {
	e, _, ctx := fixture(t)

	base, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths(base), "entries/garden.md") {
		t.Fatal("precondition failed: graph-only note absent with default tuning")
	}

	e.Tuning = query.Tuning{DisableGraph: true}
	got, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if contains(paths(got), "entries/garden.md") {
		t.Errorf("graph-only note still present with DisableGraph: %v", paths(got))
	}
}

// TopK from Tuning applies when the caller doesn't ask, and loses to
// opts.TopK when they do. Options is the caller surface; Tuning is the
// harness surface.
func TestTuningTopKPrecedence(t *testing.T) {
	e, _, ctx := fixture(t)

	e.Tuning = query.Tuning{TopK: 2}
	got, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Tuning.TopK=2 gave %d results, want 2", len(got))
	}

	got, err = e.Run(ctx, queryText, query.Options{TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("opts.TopK=3 must win over Tuning.TopK=2, got %d results", len(got))
	}
}

// CandidatePool caps each leg before fusion, so shrinking it to 1 must not
// error and must not return more than the legs can supply.
func TestTuningCandidatePoolApplies(t *testing.T) {
	e, _, ctx := fixture(t)
	e.Tuning = query.Tuning{CandidatePool: 1}
	got, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Three legs contributing at most 1 candidate each, deduped by chunk.
	if len(got) > 3 {
		t.Errorf("CandidatePool=1 across 3 legs gave %d results, want <= 3: %v", len(got), paths(got))
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
