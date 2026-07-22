package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enqack/cognosis/internal/vault"
)

func TestPassiveRefreshBudgetExhausts(t *testing.T) {
	e, _, root, ctx := testEngine(t)

	// Anchored within the budget (100d since the last explicit reinforce) but
	// ancient by last hand edit: citation is the only thing keeping it in notes/.
	writeSpec(t, root, "notes/theory.md", noteSpec{
		confidence: "0.4", stability: "14.00",
		created:      now.Add(-300 * 24 * time.Hour),
		updated:      now.Add(-300 * 24 * time.Hour),
		lastExplicit: now.Add(-100 * 24 * time.Hour),
	})
	// A healthy citer, rewritten fresh each pass so it never archives itself.
	citer := func(at time.Time) {
		writeSpec(t, root, "notes/citer.md", noteSpec{
			confidence: "1.0", stability: "14.00",
			created: at.Add(-2 * time.Hour), updated: at.Add(-time.Hour), lastExplicit: at.Add(-time.Hour),
			body: "Still building on [[theory]] today.\n",
		})
	}

	// In budget: citation shields the ancient-archival move.
	citer(now)
	r, err := e.Run(ctx, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Actions {
		if a.Note == "theory" && strings.HasPrefix(a.Kind, "archived") {
			t.Fatalf("cited note archived while in budget: %v", kinds(r))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "theory.md")); err != nil {
		t.Fatalf("note left notes/ while cited and in budget: %v", err)
	}

	// Past the budget (190d since the last explicit reinforce): citation no
	// longer shields, and the ancient note is archived. Silence stops counting
	// as assent at exactly one horizon.
	past := now.Add(90 * 24 * time.Hour)
	citer(past)
	r, err = e.Run(ctx, Options{Now: past})
	if err != nil {
		t.Fatal(err)
	}
	archived := false
	for _, a := range r.Actions {
		if a.Note == "theory" && a.Kind == "archived-ancient" {
			archived = true
		}
	}
	if !archived {
		t.Fatalf("cited note not archived after the budget expired: %v", kinds(r))
	}
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

// Decay is a pure function of time since the anchor, not an accumulation, so
// running the compile pass many times at the same instant must not compound the
// decay. This is the read-time model's central guarantee: the decay rate cannot
// depend on how often the agent happens to compile.
func TestDecayIsTimeBasedNotPerRun(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{
		confidence: "0.9", stability: "14.00", lastExplicit: now.Add(-60 * 24 * time.Hour),
	})
	if _, err := e.Run(ctx, Options{Now: now}); err != nil {
		t.Fatal(err)
	}
	after1 := toFloat(reparse(t, root, "notes/theory.md").Frontmatter["confidence"])
	if after1 >= 0.9 {
		t.Errorf("confidence %v -- the note never decayed at all", after1)
	}
	// Two more runs at the same instant: the value must not move.
	for range 2 {
		if _, err := e.Run(ctx, Options{Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	after3 := toFloat(reparse(t, root, "notes/theory.md").Frontmatter["confidence"])
	if after1 != after3 {
		t.Errorf("confidence %v after one run, %v after three: decay compounded per run "+
			"instead of tracking time", after1, after3)
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
