// Package lifecycle runs the knowledge compilation pass over notes/*:
// reinforce → decay/refresh → archive → graduate, plus the explicit
// falsify/dispute moves. Mechanics ported from silo-kb with the Cognosis
// adaptations: staleness comes from explicit frontmatter timestamps (never
// git age), archive is the archive/ stage folder, and graduation is in-place
// canonization (a graduated_at stamp exempts a stable note from decay — the
// layout has no canon folder, so canon is a frontmatter fact).
//
// Every run holds the whole-KB advisory lock, ends in exactly one history
// commit, and appends its report to the vault's append-only log.md. A dry
// run computes the same report and writes nothing at all.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
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
	// use — its decay clock resets without an explicit reinforce.
	refreshWithin = 7 * 24 * time.Hour
	stableMinRuns = 3
	developingAt  = 0.8
	stableAt      = 0.9
)

// Options is one run's explicit, agent-justified operation set.
type Options struct {
	Reinforce []string          // note ids or vault-relative paths
	Falsify   map[string]string // id/path -> reason determined false
	Dispute   map[string]string // id/path -> reason contested (live, not disproven)
	Supersede map[string]string // falsified id/path -> replacement id/path
	Graduate  []string          // ids/paths to canonize in place (must be stable)
	// Verify runs a retrieval pass over each falsify/graduate target before
	// the run's effects land, surfacing the top related notes as advisory
	// related-context report lines — the agent sees potential contradictions
	// before terminal moves. Advisory only; never blocks, requires the
	// engine's Query to be wired.
	Verify bool
	DryRun bool
	Now    time.Time
}

// Action is one lifecycle event in a run's report.
type Action struct {
	Kind   string // reinforced | refreshed | decayed | archived-faded | archived-ancient | falsified | disputed | dispute-cleared | graduated | promoted
	Note   string // wikilink basename
	Detail string
}

// Report is what a run did (or would do, for a dry run).
type Report struct {
	When             time.Time
	DryRun           bool
	Actions          []Action
	StableCandidates []string // stable, ungraduated — next run's shortlist
}

func (r *Report) String() string {
	var b strings.Builder
	suffix := ""
	if r.DryRun {
		suffix = " (dry run)"
	}
	fmt.Fprintf(&b, "## %s compile%s\n", r.When.Format(vault.TimeLayout), suffix)
	if len(r.Actions) == 0 {
		b.WriteString("- no changes\n")
	}
	for _, a := range r.Actions {
		fmt.Fprintf(&b, "- %s: [[%s]] %s\n", a.Kind, a.Note, a.Detail)
	}
	if len(r.StableCandidates) > 0 {
		links := make([]string, len(r.StableCandidates))
		for i, c := range r.StableCandidates {
			links[i] = "[[" + c + "]]"
		}
		fmt.Fprintf(&b, "- stable, ungraduated (graduation candidates): %s\n", strings.Join(links, ", "))
	}
	return b.String()
}

// Engine wires the run to the vault, the index, and the history repo.
type Engine struct {
	Store    *store.Store
	Indexer  *write.Indexer
	VaultDir string
	Hist     *vault.History
	Supp     write.Suppressor
	Log      *slog.Logger
	// Query, when set, powers Options.Verify's retrieval-augmented pass.
	Query *query.Engine
}

// relatedContext retrieves the notes most related to a terminal-move target
// and renders them as advisory report lines. Failures degrade to a single
// advisory note — verification must never block a run.
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

	// id/path -> wikilink basename for supersede targets.
	wikiByKey := map[string]string{}
	for _, n := range notes {
		wikiByKey[n.ID()] = wikiname(n.Path)
		wikiByKey[n.Path] = wikiname(n.Path)
	}

	// Every requested id/path must resolve to a live decaying note — a typo
	// silently doing nothing would let an agent report work that never
	// happened. Falsified notes are lifecycle-terminal and excluded.
	live := map[string]bool{}
	for _, n := range notes {
		if n.Stage == vault.StageNote && n.Status() != vault.StatusFalsified {
			live[n.ID()] = true
			live[n.Path] = true
		}
	}
	var unknown []string
	check := func(what string, keys map[string]bool) {
		for k := range keys {
			if !live[k] {
				unknown = append(unknown, what+" "+k)
			}
		}
	}
	check("reinforce", reinforce)
	check("graduate", graduate)
	check("falsify", toKeys(opts.Falsify))
	check("dispute", toKeys(opts.Dispute))
	for k, repl := range opts.Supersede {
		if _, ok := opts.Falsify[k]; !ok {
			unknown = append(unknown, "supersede "+k+" (no matching falsify)")
		}
		if _, ok := wikiByKey[repl]; !ok {
			unknown = append(unknown, "supersede replacement "+repl)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, cogerr.Ef(op, cogerr.Validation,
			"no live decaying note matches:\n  %s\n(ids/paths must name a note under notes/)",
			strings.Join(unknown, "\n  "))
	}

	// Reinforcing and falsifying/disputing the same note in one run is a
	// contradiction the invoking agent must resolve — never silently pick a
	// winner by loop order.
	idOf := map[string]string{}
	for _, n := range notes {
		idOf[n.ID()] = n.ID()
		idOf[n.Path] = n.ID()
	}
	byNote := map[string][]string{}
	for _, m := range []struct {
		name string
		keys map[string]bool
	}{
		{"reinforce", reinforce},
		{"falsify", toKeys(opts.Falsify)},
		{"dispute", toKeys(opts.Dispute)},
	} {
		for k := range m.keys {
			if id, ok := idOf[k]; ok {
				byNote[id] = appendUnique(byNote[id], m.name)
			}
		}
	}
	var conflicts []string
	for id, ops := range byNote {
		if len(ops) > 1 {
			sort.Strings(ops)
			conflicts = append(conflicts, id+": "+strings.Join(ops, " + "))
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, cogerr.Ef(op, cogerr.Validation,
			"contradictory operations on the same note in one run (pick one):\n  %s",
			strings.Join(conflicts, "\n  "))
	}

	// Passive citation-refresh: a note cited by a note updated recently is
	// still in use; its decay clock resets. Confidence never rises this way —
	// only an agent's reinforce asserts belief.
	idByName := map[string]string{}
	for _, n := range notes {
		if _, ok := idByName[wikiname(n.Path)]; !ok {
			idByName[wikiname(n.Path)] = n.ID()
		}
	}
	recentlyCited := map[string]bool{}
	for _, m := range notes {
		upd, err := vault.TimeOf(m.Frontmatter["updated"])
		if err != nil || opts.Now.Sub(upd) > refreshWithin {
			continue
		}
		for _, ref := range vault.Targets(m) {
			if id, ok := idByName[ref.Name]; ok && id != m.ID() {
				recentlyCited[id] = true
			}
		}
	}

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

		// Falsify: explicit, terminal, in place — wrong, not forgotten.
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
				detail += " → [[" + wikiByKey[repl] + "]]"
			}
			report.Actions = append(report.Actions, Action{"falsified", name, detail})
			if !opts.DryRun {
				if err := e.rewrite(ctx, n, n.Path); err != nil {
					return nil, err
				}
				changedFiles++
			}
			continue
		}

		// Dispute: explicit, non-terminal — contested, keeps decaying. A later
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
				if err := e.rewrite(ctx, n, n.Path); err != nil {
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
		shielded := paused || recentlyCited[n.ID()] || graduatedAlready
		clearDispute := false
		changed := false

		// Reinforce (wins over decay in the same run).
		if reinforced {
			old := conf
			// Confidence is a one-decimal quantity; round after arithmetic so
			// 0.7+0.1 actually reaches the 0.8 promotion threshold instead of
			// landing at 0.7999….
			conf = round1(min(1.0, conf+reinforceDelta))
			count++
			last = opts.Now
			changed = true
			report.Actions = append(report.Actions, Action{"reinforced", name, fmt.Sprintf("%.1f→%.1f", old, conf)})
			if n.Status() == "disputed" {
				clearDispute = true
				report.Actions = append(report.Actions, Action{"dispute-cleared", name, "(reinforced)"})
			}
			// Maturity only advances on reinforcement.
			if maturity == "seed" && conf >= developingAt {
				maturity = "developing"
				report.Actions = append(report.Actions, Action{"promoted", name, "seed→developing"})
			} else if maturity == "developing" && conf >= stableAt && count >= stableMinRuns {
				maturity = "stable"
				report.Actions = append(report.Actions, Action{"promoted", name, "developing→stable"})
			}
		} else if opts.Now.Sub(last) > staleAfter {
			if shielded {
				cause := "cited recently"
				if paused {
					cause = "paused"
				}
				if graduatedAlready {
					cause = "graduated canon"
				}
				last = opts.Now
				changed = true
				report.Actions = append(report.Actions, Action{"refreshed", name, "(" + cause + ")"})
			} else {
				old := conf
				conf = round1(max(0.0, conf-decayDelta))
				changed = true
				report.Actions = append(report.Actions, Action{"decayed", name,
					fmt.Sprintf("%.1f→%.1f (last_reinforced %s)", old, conf, last.Format(vault.TimeLayout))})
			}
		}

		if changed {
			n.SetFM("confidence", fmt.Sprintf("%.1f", conf))
			n.SetFM("maturity", maturity)
			n.SetFM("last_reinforced", last.Format(vault.TimeLayout))
			n.SetFM("reinforce_count", strconv.Itoa(count))
			if clearDispute {
				n.SetFM("status", "active")
				n.DeleteFM("disputed_reason")
				n.DeleteFM("disputed_at")
			}
			if !opts.DryRun {
				if err := e.rewrite(ctx, n, n.Path); err != nil {
					return nil, err
				}
				changedFiles++
			}
		}

		// Archive: faded (confidence hit zero) or ancient (updated timestamp
		// abandoned). Both are moves into archive/; the id survives.
		if conf <= 0 {
			report.Actions = append(report.Actions, Action{"archived-faded", name, fmt.Sprintf("(confidence %.1f)", conf)})
			if !opts.DryRun {
				n.SetFM("status", vault.StatusFaded)
				n.SetFM("archived_at", opts.Now.Format(vault.TimeLayout))
				if err := e.move(ctx, n, "archive/"+filepath.Base(n.Path)); err != nil {
					return nil, err
				}
				changedFiles++
			}
			continue
		}
		if !reinforced && !shielded && opts.Now.Sub(updated) > ancientAfter {
			report.Actions = append(report.Actions, Action{"archived-ancient", name,
				fmt.Sprintf("(updated %s ago)", opts.Now.Sub(updated).Round(24*time.Hour))})
			if !opts.DryRun {
				n.SetFM("status", vault.StatusArchived)
				n.SetFM("archived_at", opts.Now.Format(vault.TimeLayout))
				if err := e.move(ctx, n, "archive/"+filepath.Base(n.Path)); err != nil {
					return nil, err
				}
				changedFiles++
			}
			continue
		}

		// Graduate: explicit in-place canonization. Prerequisites ported:
		// stable, not paused, not disputed (contested theory must not become
		// canon — a reinforce clears a dispute).
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
				if err := e.rewrite(ctx, n, n.Path); err != nil {
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
