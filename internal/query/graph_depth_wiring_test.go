package query

import "testing"

// TestGraphDepthDecayZeroValueMatchesConstants guards the spreading-activation
// seam the same way TestTuningZeroValueMatchesConstants guards the rest: the
// zero Tuning must resolve to exactly the shipped one-hop constants, so an
// Engine that never sets GraphDepth/GraphDecay drives store.RankGraphMulti at
// depth 1 -- byte-identical to the shipped RankGraph.
func TestGraphDepthDecayZeroValueMatchesConstants(t *testing.T) {
	var zero Tuning
	if got := zero.graphDepth(); got != graphDepth {
		t.Errorf("zero.graphDepth() = %d, want %d", got, graphDepth)
	}
	if got := zero.graphDepth(); got != 1 {
		t.Errorf("zero.graphDepth() = %d, want the shipped one-hop depth 1", got)
	}
	if got := zero.graphDecay(); got != graphDecay {
		t.Errorf("zero.graphDecay() = %v, want %v", got, graphDecay)
	}
}

// A positive GraphDepth is the override; zero falls back to the shipped depth.
// There is no negative sentinel -- depth's floor equals its default -- so this
// mirrors candidatePool, not graphWeight.
func TestGraphDepthOverrides(t *testing.T) {
	if got := (Tuning{GraphDepth: 2}).graphDepth(); got != 2 {
		t.Errorf("graphDepth() = %d, want 2", got)
	}
	if got := (Tuning{GraphDepth: 0}).graphDepth(); got != graphDepth {
		t.Errorf("graphDepth() = %d with 0, want default %d", got, graphDepth)
	}
}

// GraphDecay mirrors GraphWeight's negative-is-zero sentinel: a positive value
// takes effect, zero falls back to the default, and a negative override means
// decay zero -- which zero the field cannot express, since zero means "unset".
func TestGraphDecayOverrides(t *testing.T) {
	if got := (Tuning{GraphDecay: 0.3}).graphDecay(); got != 0.3 {
		t.Errorf("graphDecay() = %v, want 0.3", got)
	}
	if got := (Tuning{GraphDecay: 0}).graphDecay(); got != graphDecay {
		t.Errorf("graphDecay() = %v with 0, want default %v", got, graphDecay)
	}
	if got := (Tuning{GraphDecay: -1}).graphDecay(); got != 0 {
		t.Errorf("graphDecay() = %v with -1, want 0 (negative is the decay-zero sentinel)", got)
	}
}

// Explicitly setting the shipped constants must be indistinguishable from the
// zero value -- the two spellings of "one-hop production" must agree.
func TestGraphDepthDecayExplicitConstantsMatchZero(t *testing.T) {
	explicit := Tuning{GraphDepth: graphDepth, GraphDecay: graphDecay}
	var zero Tuning
	if explicit.graphDepth() != zero.graphDepth() || explicit.graphDecay() != zero.graphDecay() {
		t.Errorf("explicit-constants differ from zero: explicit depth=%d decay=%v, zero depth=%d decay=%v",
			explicit.graphDepth(), explicit.graphDecay(), zero.graphDepth(), zero.graphDecay())
	}
}
