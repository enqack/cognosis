// Package lifecycle runs the knowledge compilation pass over notes/*:
// reinforce -> decay/refresh -> archive -> graduate, plus the explicit
// falsify/dispute moves. Mechanics ported from silo-kb with the Cognosis
// adaptations: staleness comes from explicit frontmatter timestamps (never
// git age), archive is the archive/ stage folder, and graduation is in-place
// canonization (a graduated_at stamp exempts a stable note from decay -- the
// layout has no canon folder, so canon is a frontmatter fact).
//
// Every run holds the whole-KB advisory lock, ends in exactly one history
// commit, and appends its report to the vault's append-only log.md. A dry
// run computes the same report and writes nothing at all.
package lifecycle

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

const (
	// Read-time decay curve: confidence(t) = (1 + t/S)^-decayExponent, where t is
	// the time since last_explicit_reinforce and S is the note's stability in
	// days. The curve is evaluated fresh from time alone every run -- there is no
	// per-run accumulation, so decay no longer depends on how often the compile
	// pass happens to run (the defect the old flat-then-staircase model had). A
	// power law, not an exponential (b=0.5 is the Ebbinghaus fit), so
	// well-reinforced knowledge keeps a long tail instead of falling off a cliff.
	decayExponent = 0.5
	// freshStability is S for a brand-new, never-reinforced note (days). It sets
	// how volatile an unreinforced note is: at 14d a fresh note reads ~0.56 at
	// 30d and half-lives at 42d -- volatile enough to signal "reinforce me", not
	// so volatile that a hard-won note evaporates before it is revisited.
	freshStability = 14.0
	// stabilityGrowth multiplies S on every explicit reinforce -- the spacing
	// effect. One reinforce roughly doubles a note's half-life, so the interval
	// to the next needed reinforcement widens as belief is repeatedly asserted.
	stabilityGrowth = 1.9
	// stableStabilityFactor multiplies S once on promotion to `stable`: semantic
	// consolidation. Canon gets an effectively permanent tail.
	stableStabilityFactor = 4.0
	// archiveBelow is the (unrounded) confidence at which a faded note is
	// archived. At freshStability and b=0.5 an unreinforced note crosses it around
	// 336 days -- close to the old model's horizon -- while a high-S note never
	// reaches it, so archival is tied to belief, and belief to reinforcement
	// history, rather than to a flat age cutoff.
	archiveBelow = 0.2

	// ancientAfter: a note whose `updated` timestamp (last hand edit, not decay)
	// is older than this is archived as abandoned -- orthogonal to belief.
	ancientAfter = 6 * 30 * 24 * time.Hour
	// refreshWithin: a note cited by a note updated this recently is still in use.
	refreshWithin = 7 * 24 * time.Hour
	// passiveRefreshBudget: citation is evidence a note is *used*, not that it is
	// *believed*. It shields the archival move only this far past the last
	// explicit reinforce; after that, silence stops counting as use and a
	// cited-but-unreinforced note may archive. Confidence itself decays from the
	// anchor regardless of citation -- citation never touches belief.
	passiveRefreshBudget = ancientAfter
	// stableMinRuns: promotion to `stable` requires at least this many reinforces.
	stableMinRuns = 3
)

// decayConfidence evaluates the read-time decay curve: confidence as a function
// of time since the last explicit reinforce and the note's stability S (days).
// A power law, so it is heavy-tailed -- a large S (well-reinforced or canonized
// note) barely moves over a year, while a fresh note (small S) is volatile.
func decayConfidence(elapsed time.Duration, stabilityDays float64) float64 {
	if stabilityDays <= 0 {
		stabilityDays = freshStability
	}
	t := elapsed.Hours() / 24.0
	if t < 0 {
		t = 0
	}
	return math.Pow(1.0+t/stabilityDays, -decayExponent)
}

// initStability reconstructs a note's stability from its reinforcement history,
// for notes written before stability was tracked. It mirrors what the compile
// pass would have accumulated -- freshStability grown once per reinforce, with
// the stable-promotion bump folded in -- so an existing developing/stable note
// starts where its history earned it rather than reset to a fresh, volatile S.
func initStability(reinforceCount int, maturity string) float64 {
	s := freshStability * math.Pow(stabilityGrowth, float64(reinforceCount))
	if maturity == "stable" {
		s *= stableStabilityFactor
	}
	return s
}

// stabilityOf reads the stored stability, or reconstructs it from history when
// absent (every note written before the read-time model). The bool reports
// whether it was stored, so the caller knows to persist a reconstructed value.
func stabilityOf(n *vault.Note, reinforceCount int, maturity string) (float64, bool) {
	if v, ok := n.Frontmatter["stability"]; ok {
		if s := toFloat(v); s > 0 {
			return s, true
		}
	}
	return initStability(reinforceCount, maturity), false
}

// passiveBudgetLeft reports how much passive-refresh budget a note has left.
// A non-positive result means citation no longer shields it.
//
// The anchor is last_explicit_reinforce, falling back to created when absent --
// which is every note written before that field existed. The fallback must not
// be last_reinforced: passive refresh *writes* that field, so anchoring to it
// would make the budget renew itself, which is the unbounded behavior this
// exists to stop. created is required, always present, and never written by
// the lifecycle, so for a note nobody ever reinforced it gives the honest
// answer -- measure from birth.
//
// A malformed anchor is treated as exhausted rather than raising: this is a
// bound, and the safe failure direction for a bound is "expired". Raising
// would let one bad optional timestamp abort a whole compile run.
func passiveBudgetLeft(n *vault.Note, now time.Time) time.Duration {
	anchor, err := vault.TimeOf(n.Frontmatter["last_explicit_reinforce"])
	if err != nil {
		if anchor, err = vault.TimeOf(n.Frontmatter["created"]); err != nil {
			return 0
		}
	}
	return passiveRefreshBudget - now.Sub(anchor)
}

// Engine wires the run to the vault, the index, and the history repo.
type Engine struct {
	Store    *store.Store
	Indexer  *write.Indexer
	VaultDir string
	Hist     *vault.History
	Supp     write.Suppressor
	// Locks must be the same *write.PathLocks the Pipeline uses. Without it a
	// concurrent edit_note can silently revert a reinforce this run just wrote:
	// both writers touch the same file and only one of them was serializing.
	Locks *write.PathLocks
	Log   *slog.Logger
	// Query, when set, powers Options.Verify's retrieval-augmented pass.
	Query *query.Engine
}

// relatedContext retrieves the notes most related to a terminal-move target
// and renders them as advisory report lines. Failures degrade to a single
// advisory note -- verification must never block a run.
func (e *Engine) relatedContext(ctx context.Context, n *vault.Note, report *Report) {
	if e.Query == nil {
		report.Actions = append(report.Actions, Action{"related-context", wikiname(n.Path),
			"(verify requested but no retrieval engine is wired)"})
		return
	}
	text := strings.TrimSpace(n.Body)
	if len(text) > 500 {
		text = text[:500]
	}
	results, err := e.Query.Run(ctx, text, query.Options{TopK: 4})
	if err != nil {
		report.Actions = append(report.Actions, Action{"related-context", wikiname(n.Path),
			"(verification query failed: " + err.Error() + ")"})
		return
	}
	var related []string
	for _, r := range results {
		if r.Path == n.Path {
			continue // the target itself
		}
		related = append(related, "[["+wikiname(r.Path)+"]]")
		if len(related) == 3 {
			break
		}
	}
	if len(related) == 0 {
		return
	}
	report.Actions = append(report.Actions, Action{"related-context", wikiname(n.Path),
		"review before this becomes final: " + strings.Join(related, ", ")})
}

// Run executes one compilation pass under the whole-KB advisory lock.
func (e *Engine) Run(ctx context.Context, opts Options) (*Report, error) {
	release, err := e.Store.AcquireAdvisory(ctx, store.LockCompile)
	if err != nil {
		return nil, err
	}
	defer release()
	return e.run(ctx, opts)
}
