package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/vault"
)

func (e *Engine) run(ctx context.Context, opts Options) (*Report, error) {
	const op = "lifecycle.Run"
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	notes, err := vault.Walk(e.VaultDir)
	if err != nil {
		return nil, err
	}

	reinforce := toSet(opts.Reinforce)
	graduate := toSet(opts.Graduate)

	wikiByKey, err := resolveTargets(notes, opts)
	if err != nil {
		return nil, err
	}
	recentlyCited := recentCitations(notes, opts.Now)

	report := &Report{When: opts.Now, DryRun: opts.DryRun}
	changedFiles := 0

	for _, n := range notes {
		if n.Stage != vault.StageNote {
			continue
		}
		if n.Status() == vault.StatusFalsified {
			continue // retained, queryable, lifecycle-inert
		}
		name := wikiname(n.Path)
		// Where this note's actions start. A skipped write truncates back to
		// here, so the report never claims something the run did not do -- one
		// reinforce can append three actions before the write that carries them.
		mark := len(report.Actions)

		// Falsify: explicit, terminal, in place -- wrong, not forgotten.
		if reason, ok := pick(opts.Falsify, n.ID(), n.Path); ok {
			if strings.TrimSpace(reason) == "" {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: refusing to falsify without a reason", n.Path)
			}
			if opts.Verify {
				e.relatedContext(ctx, n, report)
			}
			detail := "(" + reason + ")"
			n.SetFM("status", vault.StatusFalsified)
			n.SetFM("falsified_reason", reason)
			n.SetFM("falsified_at", opts.Now.Format(vault.TimeLayout))
			if repl, ok := pick(opts.Supersede, n.ID(), n.Path); ok {
				n.SetFM("superseded_by", "[["+wikiByKey[repl]+"]]")
				detail += " -> [[" + wikiByKey[repl] + "]]"
			}
			report.Actions = append(report.Actions, Action{"falsified", name, detail})
			if !opts.DryRun {
				switch err := e.rewrite(ctx, n, n.Path); {
				case errors.Is(err, ErrChangedDuringRun):
					// Somebody wrote this note while the run was in flight.
					// Report it and move on: the lifecycle is idempotent, so
					// whatever was due is due again next run against the note
					// as it now is.
					// Replace this note's action rather than adding a second
					// one. Leaving both makes the report say the note was
					// falsified/decayed/archived *and* skipped, and an agent
					// that asked for an explicit falsify has no reason to
					// cross-reference a later line for the same name -- it would
					// read a success for something that never happened.
					report.replaceSince(mark, Action{"skipped", name,
						"(changed during the run; not applied, re-evaluate on the next compile)"})
					continue
				case err != nil:
					return nil, err
				}
				changedFiles++
			}
			continue
		}

		// Dispute: explicit, non-terminal -- contested, keeps decaying. A later
		// reinforce clears it.
		if reason, ok := pick(opts.Dispute, n.ID(), n.Path); ok {
			if strings.TrimSpace(reason) == "" {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: refusing to dispute without a reason", n.Path)
			}
			n.SetFM("status", "disputed")
			n.SetFM("disputed_reason", reason)
			n.SetFM("disputed_at", opts.Now.Format(vault.TimeLayout))
			report.Actions = append(report.Actions, Action{"disputed", name, "(" + reason + ")"})
			if !opts.DryRun {
				switch err := e.rewrite(ctx, n, n.Path); {
				case errors.Is(err, ErrChangedDuringRun):
					// Somebody wrote this note while the run was in flight.
					// Report it and move on: the lifecycle is idempotent, so
					// whatever was due is due again next run against the note
					// as it now is.
					// Replace this note's action rather than adding a second
					// one. Leaving both makes the report say the note was
					// falsified/decayed/archived *and* skipped, and an agent
					// that asked for an explicit falsify has no reason to
					// cross-reference a later line for the same name -- it would
					// read a success for something that never happened.
					report.replaceSince(mark, Action{"skipped", name,
						"(changed during the run; not applied, re-evaluate on the next compile)"})
					continue
				case err != nil:
					return nil, err
				}
				changedFiles++
			}
			continue
		}

		conf := toFloat(n.Frontmatter["confidence"])
		count := toInt(n.Frontmatter["reinforce_count"])
		maturity, _ := n.Frontmatter["maturity"].(string)
		last, err := vault.TimeOf(n.Frontmatter["last_reinforced"])
		if err != nil {
			return nil, cogerr.Ef(op, cogerr.Validation, "%s: last_reinforced: %v", n.Path, err)
		}
		updated, err := vault.TimeOf(n.Frontmatter["updated"])
		if err != nil {
			return nil, cogerr.Ef(op, cogerr.Validation, "%s: updated: %v", n.Path, err)
		}

		reinforced := reinforce[n.ID()] || reinforce[n.Path]
		_, graduatedAlready := n.Frontmatter["graduated_at"]
		paused := n.Status() == "paused"

		// Two shields, not one, because the two sites they gate mean different
		// things. Citation shielding is budgeted for decay (a reversible
		// confidence slide) but not for archival (a file move): decay keys on
		// belief, archival keys on `updated`, which the lifecycle never writes
		// and which is therefore a true last-agent-edit signal. A note that is
		// cited AND was edited recently is not abandoned by any reading, and a
		// move triggered by the mere absence of a reinforce is the transition
		// most worth requiring two independent signals for.
		budgetLeft := passiveBudgetLeft(n, opts.Now)
		citedAndInBudget := recentlyCited[n.ID()] && budgetLeft > 0
		decayShielded := paused || graduatedAlready || citedAndInBudget
		moveShielded := paused || graduatedAlready || recentlyCited[n.ID()]

		clearDispute := false
		changed := false
		stampExplicit := false

		// Reinforce (wins over decay in the same run).
		if reinforced {
			old := conf
			// Confidence is a one-decimal quantity; round after arithmetic so
			// 0.7+0.1 actually reaches the 0.8 promotion threshold instead of
			// landing at 0.7999....
			conf = round1(min(1.0, conf+reinforceDelta))
			count++
			last = opts.Now
			// Only an explicit reinforce moves the budget anchor. Stamping it
			// anywhere `changed` is true (refresh, decay) would make the
			// budget renew itself -- the same self-renewing loop that
			// disqualifies last_reinforced as the anchor.
			stampExplicit = true
			changed = true
			report.Actions = append(report.Actions, Action{"reinforced", name, fmt.Sprintf("%.1f->%.1f", old, conf)})
			if n.Status() == "disputed" {
				clearDispute = true
				report.Actions = append(report.Actions, Action{"dispute-cleared", name, "(reinforced)"})
			}
			// Maturity only advances on reinforcement.
			if maturity == "seed" && conf >= developingAt {
				maturity = "developing"
				report.Actions = append(report.Actions, Action{"promoted", name, "seed->developing"})
			} else if maturity == "developing" && conf >= stableAt && count >= stableMinRuns {
				maturity = "stable"
				report.Actions = append(report.Actions, Action{"promoted", name, "developing->stable"})
			}
		} else if opts.Now.Sub(last) > staleAfter {
			if decayShielded {
				var cause string
				switch {
				case paused:
					cause = "paused"
				case graduatedAlready:
					cause = "graduated canon"
				default:
					// Citation is the only budgeted cause, so it is the only
					// one that can warn. Refresh is lazy -- it fires at most
					// once per staleAfter -- so a budget smaller than that
					// window means this is very likely the last refresh this
					// note gets. Say so while a reinforce can still help.
					cause = fmt.Sprintf("cited recently; %s of passive budget left",
						roundDays(budgetLeft))
					if budgetLeft < staleAfter {
						cause += fmt.Sprintf(" -- reinforce before %s or it starts decaying",
							opts.Now.Add(budgetLeft).Format(vault.TimeLayout))
					}
				}
				last = opts.Now
				changed = true
				report.Actions = append(report.Actions, Action{"refreshed", name, "(" + cause + ")"})
			} else {
				old := conf
				conf = round1(max(0.0, conf-decayDelta))
				// Reset the clock so decay steps once per staleAfter. Without
				// this the note stays stale and decays again on every
				// subsequent compile run, making the decay rate a function of
				// how often the agent happens to compile rather than of time.
				last = opts.Now
				changed = true
				detail := fmt.Sprintf("%.1f->%.1f (last_reinforced %s)", old, conf, last.Format(vault.TimeLayout))
				if recentlyCited[n.ID()] {
					// Still cited, so the shield is not missing -- it expired.
					// Without saying so, this reads as the refresh mechanism
					// having silently broken.
					detail = fmt.Sprintf("%.1f->%.1f (still cited, but the passive-refresh budget "+
						"expired -- only an explicit reinforce revives it)", old, conf)
				}
				report.Actions = append(report.Actions, Action{"decayed", name, detail})
			}
		}

		// Whether an archival move will follow. The frontmatter edits below are
		// staged either way, but the *write* is deferred when a move is coming:
		// writing here and then moving would touch the file twice, index it
		// twice, and leave an intermediate state in vault history that never
		// meaningfully existed.
		willArchive := conf <= 0 ||
			(!reinforced && !moveShielded && opts.Now.Sub(updated) > ancientAfter)

		if changed {
			n.SetFM("confidence", fmt.Sprintf("%.1f", conf))
			n.SetFM("maturity", maturity)
			n.SetFM("last_reinforced", last.Format(vault.TimeLayout))
			n.SetFM("reinforce_count", strconv.Itoa(count))
			if stampExplicit {
				n.SetFM("last_explicit_reinforce", opts.Now.Format(vault.TimeLayout))
			}
			if clearDispute {
				n.SetFM("status", "active")
				n.DeleteFM("disputed_reason")
				n.DeleteFM("disputed_at")
			}
			if !opts.DryRun && !willArchive {
				switch err := e.rewrite(ctx, n, n.Path); {
				case errors.Is(err, ErrChangedDuringRun):
					// Somebody wrote this note while the run was in flight.
					// Report it and move on: the lifecycle is idempotent, so
					// whatever was due is due again next run against the note
					// as it now is.
					// Replace this note's action rather than adding a second
					// one. Leaving both makes the report say the note was
					// falsified/decayed/archived *and* skipped, and an agent
					// that asked for an explicit falsify has no reason to
					// cross-reference a later line for the same name -- it would
					// read a success for something that never happened.
					report.replaceSince(mark, Action{"skipped", name,
						"(changed during the run; not applied, re-evaluate on the next compile)"})
					continue
				case err != nil:
					return nil, err
				}
				changedFiles++
				// This write landed. Re-seed the mark so a *later* skip in the
				// same iteration -- reinforce and graduate are both accepted for
				// one note, and each writes -- truncates only the actions
				// belonging to the write that failed. Truncating to the top of
				// the iteration would delete actions describing a change that
				// is already on disk, already indexed, and already counted,
				// leaving log.md denying it and an agent re-issuing a reinforce
				// that would then apply twice.
				mark = len(report.Actions)
			}
		}

		// Archive: faded (confidence hit zero) or ancient (updated timestamp
		// abandoned). Both are moves into archive/; the id survives. The move
		// writes the staged frontmatter above in one go.
		if conf <= 0 {
			report.Actions = append(report.Actions, Action{"archived-faded", name, fmt.Sprintf("(confidence %.1f)", conf)})
			if !opts.DryRun {
				n.SetFM("status", vault.StatusFaded)
				n.SetFM("archived_at", opts.Now.Format(vault.TimeLayout))
				switch err := e.move(ctx, n, "archive/"+filepath.Base(n.Path)); {
				case errors.Is(err, ErrChangedDuringRun):
					// Same skip contract as every other write site. These two
					// were written as fatal because move could not return this
					// error until its source-digest check was added -- making an
					// error reachable without revisiting its callers turned one
					// concurrently-edited note into an aborted run: the report
					// discarded along with the writes that already landed, no
					// log.md entry, and every mutated file left uncommitted in
					// vault history.
					report.replaceSince(mark, Action{"skipped", name,
						"(changed during the run; not applied, re-evaluate on the next compile)"})
					continue
				case err != nil:
					return nil, err
				}
				changedFiles++
			}
			continue
		}
		if !reinforced && !moveShielded && opts.Now.Sub(updated) > ancientAfter {
			report.Actions = append(report.Actions, Action{"archived-ancient", name,
				fmt.Sprintf("(updated %s ago)", opts.Now.Sub(updated).Round(24*time.Hour))})
			if !opts.DryRun {
				n.SetFM("status", vault.StatusArchived)
				n.SetFM("archived_at", opts.Now.Format(vault.TimeLayout))
				switch err := e.move(ctx, n, "archive/"+filepath.Base(n.Path)); {
				case errors.Is(err, ErrChangedDuringRun):
					// Same skip contract as every other write site. These two
					// were written as fatal because move could not return this
					// error until its source-digest check was added -- making an
					// error reachable without revisiting its callers turned one
					// concurrently-edited note into an aborted run: the report
					// discarded along with the writes that already landed, no
					// log.md entry, and every mutated file left uncommitted in
					// vault history.
					report.replaceSince(mark, Action{"skipped", name,
						"(changed during the run; not applied, re-evaluate on the next compile)"})
					continue
				case err != nil:
					return nil, err
				}
				changedFiles++
			}
			continue
		}

		// Graduate: explicit in-place canonization. Prerequisites ported:
		// stable, not paused, not disputed (contested theory must not become
		// canon -- a reinforce clears a dispute).
		if graduate[n.ID()] || graduate[n.Path] {
			if graduatedAlready {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: already graduated", n.Path)
			}
			if maturity != "stable" {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: refusing to graduate non-stable note (maturity %s)", n.Path, maturity)
			}
			if paused {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: refusing to graduate a paused note; unpause it first", n.Path)
			}
			if n.Status() == "disputed" {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: refusing to graduate a disputed note; resolve the dispute first (reinforce clears it)", n.Path)
			}
			if opts.Verify {
				e.relatedContext(ctx, n, report)
			}
			n.SetFM("graduated_at", opts.Now.Format(vault.TimeLayout))
			report.Actions = append(report.Actions, Action{"graduated", name, "(canon in place)"})
			if !opts.DryRun {
				switch err := e.rewrite(ctx, n, n.Path); {
				case errors.Is(err, ErrChangedDuringRun):
					// Somebody wrote this note while the run was in flight.
					// Report it and move on: the lifecycle is idempotent, so
					// whatever was due is due again next run against the note
					// as it now is.
					// Replace this note's action rather than adding a second
					// one. Leaving both makes the report say the note was
					// falsified/decayed/archived *and* skipped, and an agent
					// that asked for an explicit falsify has no reason to
					// cross-reference a later line for the same name -- it would
					// read a success for something that never happened.
					report.replaceSince(mark, Action{"skipped", name,
						"(changed during the run; not applied, re-evaluate on the next compile)"})
					continue
				case err != nil:
					return nil, err
				}
				changedFiles++
			}
			continue
		}

		if maturity == "stable" && !paused && !graduatedAlready {
			report.StableCandidates = append(report.StableCandidates, name)
		}
	}

	if !opts.DryRun {
		if err := e.appendLog(report); err != nil {
			return nil, err
		}
		if err := e.Hist.CommitAll(ctx, fmt.Sprintf("compile: %d action(s)", len(report.Actions))); err != nil {
			return nil, err
		}
		// Refresh the operator-facing history dashboard now that the run's
		// commit exists. A convenience projection: a failure must never fail the
		// compile.
		if err := e.Hist.WriteDashboard(ctx); err != nil && e.Log != nil {
			e.Log.Warn("history dashboard refresh failed", "err", err)
		}
		if e.Log != nil {
			e.Log.Info("compile run", "actions", len(report.Actions), "files_changed", changedFiles)
		}
	}
	return report, nil
}
