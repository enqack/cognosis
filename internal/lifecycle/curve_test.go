package lifecycle

import (
	"math"
	"testing"
	"time"
)

// The read-time decay curve and stability model are a deliberate calibration
// (see the decay-tuning session): b=0.5, freshStability=14d, growth=1.9,
// stable x4. These pins fail loudly if a constant is nudged without re-deciding
// the behavior it encodes -- the numbers are the contract, not the code.
func TestDecayCurveCalibration(t *testing.T) {
	day := 24 * time.Hour
	approx := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 0.01 {
			t.Errorf("%s = %.3f, want ~%.3f", name, got, want)
		}
	}

	// A fresh note (S=14d) reads ~0.56 at 30d and half-lives at 42d.
	approx("fresh @30d", decayConfidence(30*day, freshStability), 0.56)
	approx("fresh @42d (half-life)", decayConfidence(42*day, freshStability), 0.50)
	// At t=0 (the moment of reinforce) confidence is exactly its peak.
	approx("t=0 peak", decayConfidence(0, freshStability), 1.0)
	// The archival horizon: a fresh note crosses archiveBelow near 336d.
	approx("fresh @336d", decayConfidence(336*day, freshStability), archiveBelow)

	// Stability reconstruction from reinforcement history.
	approx("init seed", initStability(0, "seed"), 14.0)
	approx("init developing rc=2", initStability(2, "developing"), 50.54)
	approx("init stable rc=4", initStability(4, "stable"), 729.79)

	// A canonized note (large S) has a long flat tail: ~0.82 a year out, where a
	// fresh note would be near 0.06 -- far above the archival floor.
	if c := decayConfidence(365*day, initStability(4, "stable")); c < 0.80 {
		t.Errorf("stable note @1y = %.3f, want a long flat tail (>0.80)", c)
	}
	// A negative elapsed (clock skew) never exceeds the peak.
	if c := decayConfidence(-day, freshStability); c > 1.0 {
		t.Errorf("negative elapsed gave confidence %.3f > 1.0", c)
	}
}
