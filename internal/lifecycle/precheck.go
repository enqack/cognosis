package lifecycle

import (
	"sort"
	"strings"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/vault"
)

// resolveTargets validates one run's requested operations against the live
// vault and returns the id/path -> wikilink basename map used to render
// supersede targets.
func resolveTargets(notes []*vault.Note, opts Options) (map[string]string, error) {
	const op = "lifecycle.Run"
	reinforce := toSet(opts.Reinforce)
	graduate := toSet(opts.Graduate)

	// id/path -> wikilink basename for supersede targets.
	wikiByKey := map[string]string{}
	for _, n := range notes {
		wikiByKey[n.ID()] = wikiname(n.Path)
		wikiByKey[n.Path] = wikiname(n.Path)
	}

	// Every requested id/path must resolve to a live decaying note -- a typo
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
	// contradiction the invoking agent must resolve -- never silently pick a
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
	return wikiByKey, nil
}

// recentCitations reports which note ids are cited by a note updated within
// refreshWithin of now. Passive citation-refresh: a note cited by a note
// updated recently is still in use; its decay clock resets. Confidence never
// rises this way -- only an agent's reinforce asserts belief.
func recentCitations(notes []*vault.Note, now time.Time) map[string]bool {
	idByName := map[string]string{}
	for _, n := range notes {
		if _, ok := idByName[wikiname(n.Path)]; !ok {
			idByName[wikiname(n.Path)] = n.ID()
		}
	}
	recentlyCited := map[string]bool{}
	for _, m := range notes {
		upd, err := vault.TimeOf(m.Frontmatter["updated"])
		if err != nil || now.Sub(upd) > refreshWithin {
			continue
		}
		for _, ref := range vault.Targets(m) {
			// Body wikilinks only. A `sources:` entry is provenance -- where a
			// note came from -- and provenance never changes after the note is
			// written, so treating it as a citation makes it a permanent
			// one-sided shield driven by an unrelated file's `updated` stamp.
			// "Still in use" has to mean somebody referred to it, not that it
			// once had a parent.
			if ref.Kind != vault.Wikilink {
				continue
			}
			if id, ok := idByName[ref.Name]; ok && id != m.ID() {
				recentlyCited[id] = true
			}
		}
	}
	return recentlyCited
}
