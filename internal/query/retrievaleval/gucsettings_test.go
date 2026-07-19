package retrievaleval

import "testing"

// These are pure in-memory consistency checks over the sweep configuration.
// They deliberately do NOT call requireEval: they need no corpus, no database
// and no index, they run in microseconds, and they guard the exact desync that
// silently voided a sweep once already. That makes them CI-tier.

// The sweeps look up results by name (`recall[scope][baselineSetting]`). A
// missing key reads as the zero value rather than failing, so a rename in
// gucSettings turns every comparison into "0.000 < baseline" -- a confident,
// plausible, wrong failure. This is the check that makes the pairing real.
func TestBaselineSettingExists(t *testing.T) {
	for _, gs := range gucSettings {
		if gs.Name == baselineSetting {
			return
		}
	}
	names := make([]string, 0, len(gucSettings))
	for _, gs := range gucSettings {
		names = append(names, gs.Name)
	}
	t.Fatalf("baselineSetting %q is not in gucSettings %v -- every sweep comparison would read a "+
		"missing map key as 0 and report a false regression", baselineSetting, names)
}

// Duplicate names would silently overwrite each other in the results maps,
// so the last row would win and one configuration would go unreported.
func TestGUCSettingNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, gs := range gucSettings {
		if seen[gs.Name] {
			t.Errorf("duplicate gucSettings name %q -- results are keyed by name, so one "+
				"configuration would silently overwrite the other", gs.Name)
		}
		seen[gs.Name] = true
	}
}

// Every row must pin BOTH scan knobs. A partial SET LOCAL inherits the other
// from the session, so once store.Connect started setting them, a row named
// "ef_search=200" was really measuring ef_search=200 + relaxed_order -- 0.985
// where the isolated setting measures 0.881. The baseline row is the one that
// must be pinned hardest; the "inherits Connect" row is the deliberate
// exception that documents what the session default actually is.
func TestGUCSettingsPinBothKnobs(t *testing.T) {
	const inheritsRow = "session(inherits Connect)"
	for _, gs := range gucSettings {
		if gs.Name == inheritsRow {
			if gs.Set != nil {
				t.Errorf("%q must pass nil settings -- it exists to show what the session default is",
					gs.Name)
			}
			continue
		}
		if _, ok := gs.Set["hnsw.ef_search"]; !ok {
			t.Errorf("%q does not pin hnsw.ef_search; it will inherit the session value and "+
				"measure something other than its name", gs.Name)
		}
		if _, ok := gs.Set["hnsw.iterative_scan"]; !ok {
			t.Errorf("%q does not pin hnsw.iterative_scan; it will inherit the session value and "+
				"measure something other than its name", gs.Name)
		}
	}
}
