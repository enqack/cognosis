package retrievaleval

import (
	"fmt"
	"testing"

	"github.com/enqack/cognosis/internal/query"
)

// Manual confirmation that the diversity penalty raises source diversity on the
// REAL vault -- where the 5-of-12 crowding was actually observed, and where the
// synthetic corpus's clean clusters cannot stand in. Gated on
// COGNOSIS_GRAPHTUNE_DSN (an isolated dump) + Ollama; skipped in CI. Queries by
// note summary.
func TestDiversityRealVault(t *testing.T) {
	rv := realVaultSetup(t)
	ctx, e := rv.ctx, rv.e
	queries := rv.summaryQueries(t)

	arms := []struct {
		name  string
		decay float64
	}{{"off", -1}, {"decay=0.75", 0.75}, {"decay=0.5", 0.5}, {"decay=0.25", 0.25}}

	fmt.Printf("\ndiversity on the real vault -- %d notes queried by summary, top-%d\n",
		len(queries), query.DefaultTopK)
	fmt.Printf("%-12s %10s %12s\n", "ARM", "SOURCES@8", "MOST-CROWDED")
	var offSources float64
	for _, arm := range arms {
		e.Tuning = query.Tuning{DiversityDecay: arm.decay}
		var sumSources float64
		worst := 0
		for _, q := range queries {
			res, err := e.Run(ctx, q, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			top := capK(res, query.DefaultTopK)
			sumSources += float64(distinctNotes(top))
			if c := maxPerNote(top); c > worst {
				worst = c
			}
		}
		n := float64(len(queries))
		if arm.name == "off" {
			offSources = sumSources / n
		}
		fmt.Printf("%-12s %10.2f %12d\n", arm.name, sumSources/n, worst)
	}
	e.Tuning = query.Tuning{}
	if offSources >= float64(query.DefaultTopK) {
		t.Logf("note: off-arm already spans %d notes on average; little crowding to fix on this dump", query.DefaultTopK)
	}
}

func maxPerNote(res []query.Result) int {
	c := map[string]int{}
	worst := 0
	for _, r := range res {
		c[r.Path]++
		if c[r.Path] > worst {
			worst = c[r.Path]
		}
	}
	return worst
}
