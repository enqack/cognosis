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
	"strings"
	"time"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

const (
	reinforceDelta = 0.1
	decayDelta     = 0.1
	// staleAfter: a note not reinforced for this long starts decaying.
	staleAfter = 30 * 24 * time.Hour
	// ancientAfter: a note whose `updated` timestamp is older than this is
	// archived as abandoned (frontmatter timestamps are the staleness source).
	ancientAfter = 6 * 30 * 24 * time.Hour
	// refreshWithin: a note cited by a note updated this recently is still in
	// use -- its decay clock resets without an explicit reinforce.
	refreshWithin = 7 * 24 * time.Hour
	// passiveRefreshBudget: citation is evidence a note is *used*, not that it
	// is *believed*. Passive refresh may therefore extend a note's life only
	// this far past the last explicit reinforce; after that the note decays
	// even while cited, and an agent must assert belief to revive it.
	//
	// Tied to ancientAfter deliberately, so the vault has exactly one horizon
	// at which silence stops counting as assent. That is six staleAfter
	// windows, and refresh is lazy (it fires only once a note is already
	// stale), so a cited note emits roughly six `refreshed` lines -- each
	// carrying the remaining budget -- before it starts decaying.
	passiveRefreshBudget = ancientAfter
	stableMinRuns        = 3
	developingAt         = 0.8
	stableAt             = 0.9
)

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
