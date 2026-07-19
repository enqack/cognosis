package lifecycle

import (
	"fmt"
	"strings"
	"time"

	"github.com/enqack/cognosis/internal/vault"
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
	// related-context report lines -- the agent sees potential contradictions
	// before terminal moves. Advisory only; never blocks, requires the
	// engine's Query to be wired.
	Verify bool
	DryRun bool
	Now    time.Time
}

// Action is one lifecycle event in a run's report.
type Action struct {
	Kind   string // reinforced | refreshed | decayed | archived-faded | archived-ancient | falsified | disputed | dispute-cleared | graduated | promoted | skipped
	Note   string // wikilink basename
	Detail string
}

// Report is what a run did (or would do, for a dry run).
type Report struct {
	When             time.Time
	DryRun           bool
	Actions          []Action
	StableCandidates []string // stable, ungraduated -- next run's shortlist
}

// replaceSince drops every action recorded since mark and appends one in their
// place. mark is captured at the top of a note's iteration, so this removes
// exactly what that note contributed.
//
// Replacing only the *last* action was not enough: one reinforce can append
// three -- `reinforced`, `dispute-cleared`, `promoted` -- before the write that
// then gets skipped. Swapping the tail left `reinforced 0.7->0.8` standing above
// a `skipped` line for the same note, so the report affirmed a reinforce that
// never reached disk. That is the failure this whole mechanism exists to
// prevent, left in place on the highest-traffic path.
//
// Indexing by mark rather than matching on the note name also removes the
// question of whether two notes can share a name in one run.
func (r *Report) replaceSince(mark int, a Action) {
	if mark < 0 || mark > len(r.Actions) {
		return
	}
	r.Actions = append(r.Actions[:mark], a)
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
