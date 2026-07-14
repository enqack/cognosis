package query

import "testing"

func TestFuseRRFOrdersByScore(t *testing.T) {
	legA := Leg[string]{Items: []string{"x", "y"}, Weight: 1}
	legB := Leg[string]{Items: []string{"y", "z"}, Weight: 1}
	out := FuseRRF(60, func(s string) string { return s }, []Leg[string]{legA, legB})

	if len(out) != 3 {
		t.Fatalf("fused = %d items", len(out))
	}
	// y: 1/62 + 1/61 > x: 1/61 > z: 1/62
	if out[0].Item != "y" || out[1].Item != "x" || out[2].Item != "z" {
		t.Fatalf("order = %v", []string{out[0].Item, out[1].Item, out[2].Item})
	}
}

func TestFuseRRFWeights(t *testing.T) {
	primary := Leg[string]{Items: []string{"a"}, Weight: 1}
	booster := Leg[string]{Items: []string{"b"}, Weight: 0.5}
	out := FuseRRF(60, func(s string) string { return s }, []Leg[string]{primary, booster})
	if out[0].Item != "a" {
		t.Fatalf("weighted order wrong: %v", out[0].Item)
	}
	if got, want := out[1].Score, 0.5/61.0; got != want {
		t.Fatalf("booster score = %v, want %v", got, want)
	}
}

func TestFuseRRFDeterministicTies(t *testing.T) {
	leg := Leg[string]{Items: []string{"first", "second"}, Weight: 1}
	tie := Leg[string]{Items: []string{"second", "first"}, Weight: 1}
	for range 20 {
		out := FuseRRF(60, func(s string) string { return s }, []Leg[string]{leg, tie})
		if out[0].Item != "first" {
			t.Fatal("tie-break is not first-appearance-deterministic")
		}
	}
}

func TestFuseRRFDedupesByKey(t *testing.T) {
	a := Leg[string]{Items: []string{"same"}, Weight: 1}
	b := Leg[string]{Items: []string{"same"}, Weight: 1}
	out := FuseRRF(60, func(s string) string { return s }, []Leg[string]{a, b})
	if len(out) != 1 {
		t.Fatalf("dedupe failed: %d items", len(out))
	}
	if want := 2.0 / 61.0; out[0].Score != want {
		t.Fatalf("score = %v, want %v", out[0].Score, want)
	}
}
