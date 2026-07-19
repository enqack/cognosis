package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/vault"
)

func TestPassiveRefreshBudgetExhausts(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	base := now.Add(-365 * 24 * time.Hour)

	// Confidence high enough to survive several decays without hitting
	// archived-faded, which would move the note out of notes/ and end the run.
	writeSpec(t, root, "notes/theory.md", noteSpec{
		created: base, lastReinf: base, lastExplicit: base, confidence: "0.9",
	})
	citerID := uuid.Must(uuid.NewV7()).String()

	var refreshed, decayed int
	for i := 1; i <= 11; i++ {
		at := base.Add(time.Duration(i) * 31 * 24 * time.Hour)
		// Re-write the citer fresh each pass so it always shields and never
		// decays into the action list itself.
		writeSpec(t, root, "notes/citer.md", noteSpec{
			id: citerID, created: base, updated: at.Add(-time.Hour),
			lastReinf: at.Add(-time.Hour), lastExplicit: at.Add(-time.Hour),
			body: "Still building on [[theory]] today.\n",
		})
		r, err := e.Run(ctx, Options{Now: at})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range r.Actions {
			if a.Note != "theory" {
				continue
			}
			switch a.Kind {
			case "refreshed":
				refreshed++
			case "decayed":
				decayed++
			}
		}
	}

	if refreshed == 0 {
		t.Error("no passive refresh happened at all; the citation shield is not working")
	}
	if decayed == 0 {
		t.Error("the note refreshed forever -- the passive-refresh budget never expired")
	}
	fm := reparse(t, root, "notes/theory.md").Frontmatter
	if got := toFloat(fm["confidence"]); got >= 0.9 {
		t.Errorf("confidence %v never dropped despite the budget expiring", got)
	}
	// The budget gates decay, not archival: a still-cited note must not be
	// silently moved out from under the agent.
	if _, err := os.Stat(filepath.Join(root, "notes", "theory.md")); err != nil {
		t.Errorf("note left notes/ -- budget expiry must decay, not archive: %v", err)
	}
	t.Logf("over 11 runs: %d refreshed, %d decayed", refreshed, decayed)
}

// A note predating last_explicit_reinforce falls back to created. One created
// beyond the budget gets no shield, which is the upgrade semantics for every
// existing vault.
func TestLegacyNoteOutsideBudgetNotShielded(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{
		created:   now.Add(-400 * 24 * time.Hour), // older than the budget
		lastReinf: now.Add(-31 * 24 * time.Hour),  // stale, so the branch is reached
		// no lastExplicit: this is a pre-existing note
	})
	writeSpec(t, root, "notes/citer.md", noteSpec{
		updated: now.Add(-time.Hour), body: "Still building on [[theory]] today.\n",
	})
	r, err := e.Run(ctx, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Actions {
		if a.Note == "theory" && a.Kind == "refreshed" {
			t.Fatalf("legacy note outside the budget was still shielded: %v", kinds(r))
		}
	}
}

// The anchor moves only on explicit reinforce. If a passive refresh moved it,
// the budget would renew itself and bound nothing.
func TestOnlyExplicitReinforceStampsAnchor(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})

	if _, err := e.Run(ctx, Options{Reinforce: []string{"notes/theory.md"}, Now: now}); err != nil {
		t.Fatal(err)
	}
	stamped, _ := vault.TimeOf(reparse(t, root, "notes/theory.md").Frontmatter["last_explicit_reinforce"])
	if stamped.IsZero() {
		t.Fatal("explicit reinforce did not stamp last_explicit_reinforce")
	}

	// A later run that only passively refreshes must leave the anchor alone.
	later := now.Add(31 * 24 * time.Hour)
	writeSpec(t, root, "notes/citer.md", noteSpec{
		updated: later.Add(-time.Hour), body: "Still building on [[theory]] today.\n",
	})
	if _, err := e.Run(ctx, Options{Now: later}); err != nil {
		t.Fatal(err)
	}
	after, _ := vault.TimeOf(reparse(t, root, "notes/theory.md").Frontmatter["last_explicit_reinforce"])
	if !after.Equal(stamped) {
		t.Errorf("passive refresh moved the anchor %v -> %v; the budget would renew itself",
			stamped, after)
	}
}

// Dispute means "contested, keeps decaying". Stamping the anchor there would
// make disputing a note extend its life -- inverting the primitive.
func TestDisputeDoesNotStampAnchor(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{})
	if _, err := e.Run(ctx, Options{
		Dispute: map[string]string{"notes/theory.md": "contested by a later finding"},
		Now:     now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, present := reparse(t, root, "notes/theory.md").Frontmatter["last_explicit_reinforce"]; present {
		t.Error("dispute stamped the budget anchor; disputing a note must not extend its life")
	}
}

// Decay must step once per staleAfter, not once per compile run. Before this,
// decay left last_reinforced untouched, so the note stayed stale and decayed
// again on every subsequent run -- making the decay rate a function of how
// often the agent happened to compile.
func TestDecayIsTimeBasedNotPerRun(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{
		confidence: "0.9", lastReinf: now.Add(-31 * 24 * time.Hour),
	})
	// Three runs inside a single staleAfter window.
	for i := range 3 {
		if _, err := e.Run(ctx, Options{Now: now.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	got := toFloat(reparse(t, root, "notes/theory.md").Frontmatter["confidence"])
	if got < 0.8 {
		t.Errorf("confidence %v -- decayed more than once in one staleAfter window "+
			"(three compile runs should not mean three decays)", got)
	}
	if got >= 0.9 {
		t.Errorf("confidence %v -- the note never decayed at all", got)
	}
}

// A `sources:` entry is provenance, not use. It must not shield its target.
func TestSourcesProvenanceDoesNotShield(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})
	// A fresh note whose ONLY reference to theory is via sources:.
	writeSpec(t, root, "notes/derived.md", noteSpec{
		updated: now.Add(-time.Hour),
		extra:   "", body: "No body link to it.\n",
	})
	// Rewrite derived.md's sources to point at theory.
	p := filepath.Join(root, "notes", "derived.md")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b),
		`  - "[[capture]]"`, `  - "[[theory]]"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := e.Run(ctx, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Actions {
		if a.Note == "theory" && a.Kind == "refreshed" {
			t.Fatalf("a sources: entry shielded its target; provenance is not use: %v", kinds(r))
		}
	}
}
