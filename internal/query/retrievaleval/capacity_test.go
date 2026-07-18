package retrievaleval

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Q1: how many candidates does the vector leg actually return, as a function
// of scope selectivity and scan settings?
//
// Assertions are bounds and relations only — never an absolute row count.
// The recorded artifact carries the numbers.
func TestVectorLegCapacity(t *testing.T) {
	requireEval(t)
	ctx := context.Background()
	spec := evalSpec(t)
	c := Build(t, spec)

	var b strings.Builder
	fmt.Fprintf(&b, "vector leg capacity — %d notes x %d chunks = %d chunks, pool=%d\n\n",
		spec.Notes, spec.ChunksPerNote, spec.Notes*spec.ChunksPerNote, 50)
	fmt.Fprintf(&b, "%-14s %-24s %9s %9s %10s  %s\n",
		"SCOPE", "SETTING", "REQUESTED", "RETURNED", "SHORT", "EMBEDDINGS ACCESS")

	// returned[scope][setting]
	returned := map[string]map[string]int{}
	hnswSeen := map[string]bool{}
	// How many of the query set came back short, per cell. The average alone
	// hides whether a cell is uniformly short or occasionally catastrophic.
	truncatedBy := map[string]int{}

	for _, scope := range c.ScopeNames() {
		filter := c.Scopes()[scope]
		returned[scope] = map[string]int{}
		for _, gs := range gucSettings {
			// Average behavior over several queries: HNSW is not
			// deterministic per-query, so a single probe is anecdote.
			var sumReturned, truncated int
			var plan string
			for _, q := range c.Queries {
				vec, err := c.Provider.EmbedQuery(ctx, q.Text)
				if err != nil {
					t.Fatal(err)
				}
				p, err := c.Store.ProbeVector(ctx, c.Table, vec, filter, 50, gs.Set, plan == "")
				if err != nil {
					t.Fatal(err)
				}
				if plan == "" {
					plan = p.Plan
				}
				sumReturned += len(p.Rows)
				if p.Truncated() {
					truncated++
				}
				if len(p.Rows) > p.Requested {
					t.Fatalf("%s/%s returned %d rows for a limit of %d",
						scope, gs.Name, len(p.Rows), p.Requested)
				}
			}
			avg := sumReturned / len(c.Queries)
			truncatedBy[scope+"/"+gs.Name] = truncated
			returned[scope][gs.Name] = avg
			if usedHNSW(plan) {
				hnswSeen[scope] = true
			}
			fmt.Fprintf(&b, "%-14s %-24s %9d %9d %9d/%d  %s\n",
				scope, gs.Name, 50, avg, truncatedBy[scope+"/"+gs.Name], len(c.Queries),
				accessPath(plan, c.Table))
		}
	}

	// InScope is keyed by project, which is not the same key space as the
	// scope names (which include the non-project scopes "all" and
	// "with_archived"). Report it as what it is.
	fmt.Fprintf(&b, "\nlive chunks by project: total=%d", c.InScope[""])
	for _, p := range c.ScopeNames() {
		if n, ok := c.InScope[p]; ok && p != "" {
			fmt.Fprintf(&b, " %s=%d", p, n)
		}
	}
	b.WriteByte('\n')
	writeArtifact(t, "leg_capacity.txt", b.String())
	t.Log("\n" + b.String())

	// The experiment is only meaningful where the planner actually used the
	// HNSW index. If it never did, the corpus is too small and every cell
	// above is a full exact answer masquerading as a healthy result.
	if len(hnswSeen) == 0 {
		t.Fatalf("no scope used the HNSW index at %d chunks; corpus too small to measure ANN behavior "+
			"(raise COGNOSIS_EVAL_NOTES)", spec.Notes*spec.ChunksPerNote)
	}

	// Relation, not magnitude: no configuration may return fewer rows than the
	// pre-fix baseline. Derived from gucSettings rather than a hardcoded name
	// list, which silently reads a missing map key as 0 and fires a false
	// failure whenever a setting is renamed.
	for scope := range hnswSeen {
		def := returned[scope][baselineSetting]
		for _, gs := range gucSettings {
			if gs.Name == baselineSetting {
				continue
			}
			if got := returned[scope][gs.Name]; got < def {
				t.Errorf("%s: %s returned %d < pre-fix baseline %d; "+
					"a larger candidate list must not lose rows", scope, gs.Name, got, def)
			}
		}
	}
}
