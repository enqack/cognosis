package query

import "testing"

// TestTuningZeroValueMatchesConstants is the non-regression guard on the
// Tuning seam: the zero value must resolve to exactly the package constants,
// so an Engine that never sets Tuning behaves identically to one from before
// the seam existed. If this fails, every golden in this package is measuring
// something other than production behavior.
func TestTuningZeroValueMatchesConstants(t *testing.T) {
	var zero Tuning
	if got := zero.rrfK(); got != rrfK {
		t.Errorf("zero.rrfK() = %d, want %d", got, rrfK)
	}
	if got := zero.candidatePool(); got != candidatePool {
		t.Errorf("zero.candidatePool() = %d, want %d", got, candidatePool)
	}
	if got := zero.graphWeight(); got != graphWeight {
		t.Errorf("zero.graphWeight() = %v, want %v", got, graphWeight)
	}
	if got := zero.topK(); got != DefaultTopK {
		t.Errorf("zero.topK() = %d, want %d", got, DefaultTopK)
	}
}

// Explicitly setting Tuning to the constants must be indistinguishable from
// leaving it zero -- the two spellings of "production defaults" must agree.
func TestTuningExplicitConstantsMatchZero(t *testing.T) {
	explicit := Tuning{
		RRFK:          rrfK,
		CandidatePool: candidatePool,
		TopK:          DefaultTopK,
		GraphWeight:   graphWeight,
	}
	var zero Tuning
	if explicit.rrfK() != zero.rrfK() ||
		explicit.candidatePool() != zero.candidatePool() ||
		explicit.graphWeight() != zero.graphWeight() ||
		explicit.topK() != zero.topK() {
		t.Errorf("explicit-constants Tuning differs from zero Tuning:\n explicit: k=%d pool=%d gw=%v topk=%d\n zero:     k=%d pool=%d gw=%v topk=%d",
			explicit.rrfK(), explicit.candidatePool(), explicit.graphWeight(), explicit.topK(),
			zero.rrfK(), zero.candidatePool(), zero.graphWeight(), zero.topK())
	}
}

// A non-default graph weight must actually take effect; 0 falls back to the
// default, because "no graph leg" is DisableGraph's job, not a zero weight's
// (a zero-weighted leg still inserts its items at score 0).
func TestTuningGraphWeightOverrides(t *testing.T) {
	if got := (Tuning{GraphWeight: 0.25}).graphWeight(); got != 0.25 {
		t.Errorf("graphWeight() = %v, want 0.25", got)
	}
	if got := (Tuning{GraphWeight: 0}).graphWeight(); got != graphWeight {
		t.Errorf("graphWeight() = %v with 0, want default %v", got, graphWeight)
	}
}

// Negative and zero scalar fields fall back to the defaults rather than
// producing a nonsensical pool of 0 (which would silently return no results).
func TestTuningNonPositiveScalarsFallBack(t *testing.T) {
	for _, tn := range []Tuning{
		{RRFK: 0, CandidatePool: 0, TopK: 0},
		{RRFK: -1, CandidatePool: -5, TopK: -3},
	} {
		if tn.rrfK() != rrfK || tn.candidatePool() != candidatePool || tn.topK() != DefaultTopK {
			t.Errorf("%+v did not fall back to defaults: k=%d pool=%d topk=%d",
				tn, tn.rrfK(), tn.candidatePool(), tn.topK())
		}
	}
}
