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

		oldConf := toFloat(n.Frontmatter["confidence"])
		count := toInt(n.Frontmatter["reinforce_count"])
		maturity, _ := n.Frontmatter["maturity"].(string)
		// The decay anchor is the last EXPLICIT reinforce; confidence is a pure
		// function of the time since it. Notes predating that field anchor from
		// creation -- the same fallback passiveBudgetLeft uses -- never from
		// last_reinforced, which the pass itself writes.
		anchor, err := vault.TimeOf(n.Frontmatter["last_explicit_reinforce"])
		if err != nil {
			if anchor, err = vault.TimeOf(n.Frontmatter["created"]); err != nil {
				return nil, cogerr.Ef(op, cogerr.Validation, "%s: last_explicit_reinforce/created: %v", n.Path, err)
			}
		}
		updated, err := vault.TimeOf(n.Frontmatter["updated"])
		if err != nil {
			return nil, cogerr.Ef(op, cogerr.Validation, "%s: updated: %v", n.Path, err)
		}
		stability, hadStability := stabilityOf(n, count, maturity)
		origStability := stability

		reinforced := reinforce[n.ID()] || reinforce[n.Path]
		_, graduatedAlready := n.Frontmatter["graduated_at"]
		paused := n.Status() == "paused"

		// Citation shields the archival MOVE (a note in use is not abandoned) but
		// never confidence: belief decays from the last explicit reinforce however
		// often the note is cited -- citation is evidence of use, not of belief.
		// moveShielded also keys on `updated`, the true last-hand-edit signal the
		// pass never writes. Confidence is frozen only for paused and graduated
		// (canon) notes, which are exempt from decay outright.
		budgetLeft := passiveBudgetLeft(n, opts.Now)
		citedAndInBudget := recentlyCited[n.ID()] && budgetLeft > 0
		moveShielded := paused || graduatedAlready || citedAndInBudget
		confFrozen := paused || graduatedAlready

		clearDispute := false
		stampExplicit := false
		promoted := "" // promotion detail, reported after the reinforced line

		// Reinforce: an assertion of belief. Stability grows (the spacing effect --
		// each review widens the interval to the next) and confidence returns to
		// its peak, because the anchor moves to now. Maturity advances here and
		// only here; promotion to stable adds a one-time consolidation bump to S.
		if reinforced {
			stability *= stabilityGrowth
			count++
			anchor = opts.Now
			stampExplicit = true
			if maturity == "seed" {
				maturity = "developing"
				promoted = "seed->developing"
			} else if maturity == "developing" && count >= stableMinRuns {
				maturity = "stable"
				stability *= stableStabilityFactor
				promoted = "developing->stable"
			}
			if n.Status() == "disputed" {
				clearDispute = true
			}
		}

		// Read-time confidence: recomputed every run from (now - anchor) and the
		// note's stability. A pure function of time, never an accumulation, so
		// decay no longer depends on how often the compile pass happens to run.
		// Frozen notes keep their stored confidence.
		rawConf := oldConf
		if !confFrozen {
			rawConf = decayConfidence(opts.Now.Sub(anchor), stability)
		}
		conf := round1(rawConf)

		changed := reinforced || conf != round1(oldConf) || !hadStability || stability != origStability

		switch {
		case reinforced:
			report.Actions = append(report.Actions, Action{"reinforced", name,
				fmt.Sprintf("%.1f->%.1f (stability %.0f->%.0fd)", oldConf, conf, origStability, stability)})
			if promoted != "" {
				report.Actions = append(report.Actions, Action{"promoted", name, promoted})
			}
			if clearDispute {
				report.Actions = append(report.Actions, Action{"dispute-cleared", name, "(reinforced)"})
			}
		case !confFrozen && conf < round1(oldConf):
			detail := fmt.Sprintf("%.1f->%.1f (stability %.0fd, %s since reinforce)",
				round1(oldConf), conf, stability, opts.Now.Sub(anchor).Round(24*time.Hour))
			if recentlyCited[n.ID()] {
				detail += " -- still cited, but citation shields archival, not belief"
			}
			report.Actions = append(report.Actions, Action{"decayed", name, detail})
		case changed:
			// First-run backfill of the stability field, or a stored confidence the
			// curve disagrees with -- a one-time realignment, not an ongoing move.
			report.Actions = append(report.Actions, Action{"recalibrated", name,
				fmt.Sprintf("%.1f->%.1f (stability %.0fd)", round1(oldConf), conf, stability)})
		}

		// A move is coming when the note faded below the archival floor (belief
		// gone, not merely low) with no live citation to shield it, or its last
		// hand edit is ancient. Frontmatter edits are staged either way; the write
		// is deferred to the move so the file is not touched and indexed twice.
		willArchiveFaded := !confFrozen && !citedAndInBudget && rawConf < archiveBelow
		willArchiveAncient := !reinforced && !moveShielded && opts.Now.Sub(updated) > ancientAfter
		willArchive := willArchiveFaded || willArchiveAncient

		if changed {
			n.SetFM("confidence", fmt.Sprintf("%.1f", conf))
			n.SetFM("maturity", maturity)
			n.SetFM("stability", fmt.Sprintf("%.2f", stability))
			n.SetFM("reinforce_count", strconv.Itoa(count))
			if stampExplicit {
				n.SetFM("last_explicit_reinforce", opts.Now.Format(vault.TimeLayout))
				n.SetFM("last_reinforced", opts.Now.Format(vault.TimeLayout))
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
		if willArchiveFaded {
			report.Actions = append(report.Actions, Action{"archived-faded", name,
				fmt.Sprintf("(confidence %.1f, faded below %.1f)", conf, archiveBelow)})
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
		if willArchiveAncient {
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
