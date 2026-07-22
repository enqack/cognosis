package retrievaleval

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/query"
)

// TestContextRelevance closes the ship/no-ship question the context-prior sweep
// left open: the prior re-ranks orthogonally to the graph leg, but does it
// re-rank BETTER? Relevance ground truth is the self-cue -- a cue drawn from a
// note's own text should return THAT note. So retrievability of the target note,
// measured WITH the prior (context = the note's own project) vs WITHOUT, is a
// direct relevance signal: if boosting the query's project rescues the target
// from cross-project crowding, retrievability rises; if it only shuffles
// neighbours, it does not. CHURN (baseline top-8 members the prior evicts) is
// the countervailing cost -- same-project crowding of otherwise-good results.
//
// Read by project: the prior is expected to help minority-project (analytica)
// targets, where majority-project notes crowd the pool, and be near-inert for
// majority (cognosis) targets. Gated on COGNOSIS_GRAPHTUNE_DSN + Ollama.
func TestContextRelevance(t *testing.T) {
	rv := realVaultSetup(t)
	ctx, e, pool := rv.ctx, rv.e, rv.pool

	notes := loadRetrievabilityNotes(ctx, t, pool)
	if len(notes) == 0 {
		t.Fatal("no notes with cues to probe")
	}

	kLevels := []int{1, 3, 8}

	// One flat cue record per (note, self-cue).
	type cue struct {
		path, project, text string
	}
	var cues []cue
	projCount := map[string]int{}
	for _, n := range notes {
		for _, c := range n.cues {
			cues = append(cues, cue{path: n.path, project: n.project, text: c})
		}
		projCount[scopeLabel(n.project)]++
	}

	// hitAtK: best 1-based rank of path in the top-kTop, and whether <= each k.
	targetRank := func(res []query.Result, path string) int {
		top := capK(res)
		for i, r := range top {
			if r.Path == path {
				return i + 1
			}
		}
		return 0
	}
	top8Set := func(res []query.Result) map[string]bool {
		m := map[string]bool{}
		for _, r := range capK(res) {
			m[r.Content] = true
		}
		return m
	}

	// Baseline pass (weight-independent): target rank + top-8 per cue.
	baseRank := make([]int, len(cues))
	baseTop8 := make([]map[string]bool, len(cues))
	for i, c := range cues {
		e.Tuning = query.Tuning{}
		res, err := e.Run(ctx, c.text, query.Options{})
		if err != nil {
			t.Fatal(err)
		}
		baseRank[i] = targetRank(res, c.path)
		baseTop8[i] = top8Set(res)
	}

	// otherProject maps a note's project to the "wrong" context for the harm arm:
	// the other main project. Global/unknown notes have no mismatched context, so
	// they are excluded from the harm measurement.
	otherProject := func(p string) string {
		switch p {
		case "cognosis":
			return "analytica"
		case "analytica":
			return "cognosis"
		default:
			return ""
		}
	}

	type acc struct {
		n        int         // cues in this scope (all arms)
		baseHit  map[int]int // target @k under baseline, over all n
		priorHit map[int]int // target @k under RIGHT context, over all n
		churn    float64     // baseline top-8 evicted by the right context
		nWrong   int         // cues with a defined wrong context
		baseHitW map[int]int // target @k under baseline, over the nWrong cues
		wrongHit map[int]int // target @k under WRONG context, over nWrong
		churnW   float64     // baseline top-8 evicted by the wrong context
	}
	newAcc := func() *acc {
		return &acc{baseHit: map[int]int{}, priorHit: map[int]int{},
			baseHitW: map[int]int{}, wrongHit: map[int]int{}}
	}

	weights := []float64{1.5, 2.0, 3.0}
	var b, hb strings.Builder
	fmt.Fprintf(&b, "context-prior RELEVANCE sweep -- REAL multi-project dump, %d notes, %d self-cues\n", len(notes), len(cues))
	fmt.Fprintf(&b, "projects (notes): ")
	for p, c := range projCount {
		fmt.Fprintf(&b, "%s=%d ", p, c)
	}
	b.WriteString("\n\nretrievability@K = fraction of self-cues that return the TARGET note in top-K.\n" +
		"base = shipped; prior = RIGHT context (target's own project).\n" +
		"dK = prior - base (positive => the prior helps the note find itself). CHURN =\n" +
		"mean baseline top-8 members the prior evicts (the crowding cost). Split by the\n" +
		"target note's project.\n\n")
	fmt.Fprintf(&b, "%-6s %-11s %7s %7s %7s %7s %7s %7s %7s\n",
		"WEIGHT", "SCOPE", "r@1", "d@1", "r@3", "d@3", "r@8", "d@8", "CHURN")

	hb.WriteString("context-prior CROSS-PROJECT HARM sweep -- REAL multi-project dump\n\n" +
		"Mirror of the relevance sweep: each self-cue is run with the WRONG context\n" +
		"(the OTHER main project), modelling 'working in project Y while the answer\n" +
		"lives in project X'. harm@K = base retrievability - wrong-context retrievability\n" +
		"(positive => a mismatched boost DEMOTED the genuinely relevant target). CHURN =\n" +
		"baseline top-8 the wrong context evicts. Over cues with a defined other project\n" +
		"(global excluded).\n\n")
	fmt.Fprintf(&hb, "%-6s %-11s %7s %7s %7s %7s %7s\n",
		"WEIGHT", "SCOPE", "r@1base", "h@1", "h@3", "h@8", "CHURN")

	scopesOrder := []string{"all", "cognosis", "analytica", "<global>"}
	for _, w := range weights {
		accs := map[string]*acc{}
		get := func(k string) *acc {
			if accs[k] == nil {
				accs[k] = newAcc()
			}
			return accs[k]
		}
		for i, c := range cues {
			// Right-context arm (benefit): context = the target's own project.
			tune := query.Tuning{}
			if c.project != "" {
				tune.ContextProject, tune.ContextWeight = c.project, w
			}
			e.Tuning = tune
			res, err := e.Run(ctx, c.text, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			pr := targetRank(res, c.path)
			pt8 := top8Set(res)
			evicted := 0
			for k := range baseTop8[i] {
				if !pt8[k] {
					evicted++
				}
			}
			for _, sc := range []string{"all", scopeLabel(c.project)} {
				a := get(sc)
				a.n++
				a.churn += float64(evicted)
				for _, k := range kLevels {
					if baseRank[i] != 0 && baseRank[i] <= k {
						a.baseHit[k]++
					}
					if pr != 0 && pr <= k {
						a.priorHit[k]++
					}
				}
			}

			// Wrong-context arm (harm): context = the OTHER main project.
			op := otherProject(c.project)
			if op == "" {
				continue
			}
			e.Tuning = query.Tuning{ContextProject: op, ContextWeight: w}
			wres, err := e.Run(ctx, c.text, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			wr := targetRank(wres, c.path)
			wt8 := top8Set(wres)
			evictedW := 0
			for k := range baseTop8[i] {
				if !wt8[k] {
					evictedW++
				}
			}
			for _, sc := range []string{"all", scopeLabel(c.project)} {
				a := get(sc)
				a.nWrong++
				a.churnW += float64(evictedW)
				for _, k := range kLevels {
					if baseRank[i] != 0 && baseRank[i] <= k {
						a.baseHitW[k]++
					}
					if wr != 0 && wr <= k {
						a.wrongHit[k]++
					}
				}
			}
		}
		for _, sc := range scopesOrder {
			a := accs[sc]
			if a == nil || a.n == 0 {
				continue
			}
			r := func(hits map[int]int, k, denom int) float64 { return float64(hits[k]) / float64(denom) }
			rb1, rb3, rb8 := r(a.baseHit, 1, a.n), r(a.baseHit, 3, a.n), r(a.baseHit, 8, a.n)
			rp1, rp3, rp8 := r(a.priorHit, 1, a.n), r(a.priorHit, 3, a.n), r(a.priorHit, 8, a.n)
			fmt.Fprintf(&b, "%-6.1f %-11s %7.3f %+7.3f %7.3f %+7.3f %7.3f %+7.3f %7.2f\n",
				w, sc, rp1, rp1-rb1, rp3, rp3-rb3, rp8, rp8-rb8, a.churn/float64(a.n))
			if a.nWrong == 0 {
				continue
			}
			bw1, bw3, bw8 := r(a.baseHitW, 1, a.nWrong), r(a.baseHitW, 3, a.nWrong), r(a.baseHitW, 8, a.nWrong)
			ww1, ww3, ww8 := r(a.wrongHit, 1, a.nWrong), r(a.wrongHit, 3, a.nWrong), r(a.wrongHit, 8, a.nWrong)
			fmt.Fprintf(&hb, "%-6.1f %-11s %7.3f %+7.3f %+7.3f %+7.3f %7.2f\n",
				w, sc, bw1, bw1-ww1, bw3-ww3, bw8-ww8, a.churnW/float64(a.nWrong))
		}
	}
	e.Tuning = query.Tuning{}

	out := b.String()
	t.Log("\n" + out + "\n" + hb.String())
	writeArtifact(t, "context_relevance_sweep.txt", out)
	writeArtifact(t, "context_crossproject_sweep.txt", hb.String())
}

func scopeLabel(project string) string {
	if project == "" {
		return "<global>"
	}
	return project
}
