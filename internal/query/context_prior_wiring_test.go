package query_test

import (
	"context"
	"testing"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/write"
)

// The context prior (Tuning.ContextProject/ContextWeight) must be a no-op when
// unset -- the request path never sets it -- and, when set, boost same-project
// chunks enough to re-rank. Two notes carry identical query vocabulary so the
// keyword leg ranks them by the (n.path, c.ordinal) tie-break alone: aaa.md
// (project "other") lands above zzz.md (project "acme"). A prior toward "acme"
// must flip zzz above aaa; no prior must leave the tie-break order intact.
func TestContextPriorReRanksByProject(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	ix := &write.Indexer{Store: s} // keyword + graph legs only; no vector to confound
	const body = "shared alpha beta gamma delta epsilon zeta"
	indexConcept(t, ctx, ix, "notes/aaa.md", "other", body)
	indexConcept(t, ctx, ix, "notes/zzz.md", "acme", body)

	e := &query.Engine{Store: s}
	const q = "shared alpha beta gamma delta epsilon zeta"

	rankOf := func(res []query.Result, path string) int {
		for i, r := range res {
			if r.Path == path {
				return i
			}
		}
		return -1
	}

	// Default (empty ContextProject): no prior, tie-break order aaa before zzz.
	base, err := e.Run(ctx, q, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a, z := rankOf(base, "notes/aaa.md"), rankOf(base, "notes/zzz.md"); a < 0 || z < 0 || a > z {
		t.Fatalf("baseline: want aaa before zzz (a<z), got a=%d z=%d", a, z)
	}

	// Prior toward acme (zzz's project) must lift zzz above aaa.
	e.Tuning = query.Tuning{ContextProject: "acme", ContextWeight: 2}
	boosted, err := e.Run(ctx, q, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a, z := rankOf(boosted, "notes/aaa.md"), rankOf(boosted, "notes/zzz.md"); a < 0 || z < 0 || z > a {
		t.Fatalf("context prior toward acme: want zzz before aaa (z<a), got a=%d z=%d", a, z)
	}

	// A prior toward a project neither note carries changes nothing vs baseline.
	e.Tuning = query.Tuning{ContextProject: "nonesuch", ContextWeight: 2}
	neutral, err := e.Run(ctx, q, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a, z := rankOf(neutral, "notes/aaa.md"), rankOf(neutral, "notes/zzz.md"); a > z {
		t.Fatalf("prior toward an absent project should not re-rank, got a=%d z=%d", a, z)
	}
}
