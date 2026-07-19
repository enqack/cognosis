package vault

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Table-driven contract tests, ported from silo-kb's validate_test where the
// rules survived the port, plus the Cognosis-specific rules (category,
// created/updated, persona/description on reflections).

func validDecayingFM() map[string]any {
	return map[string]any{
		"id":              uuid.Must(uuid.NewV7()).String(),
		"category":        "concept",
		"created":         "2026-07-12 10:00:00",
		"updated":         "2026-07-12 10:00:00",
		"confidence":      0.5,
		"maturity":        "seed",
		"last_reinforced": "2026-07-12 10:00:00",
		"reinforce_count": 0,
		"sources":         []any{"[[2026-07-12]]"},
	}
}

func fieldsOf(probs []Problem) []string {
	out := make([]string, 0, len(probs))
	for _, p := range probs {
		out = append(out, p.Field)
	}
	return out
}

func assertField(t *testing.T, probs []Problem, field string) {
	t.Helper()
	for _, p := range probs {
		if p.Field == field {
			return
		}
	}
	t.Errorf("expected a problem naming field %q, got %v", field, fieldsOf(probs))
}

func TestValidDecayingNotePasses(t *testing.T) {
	if probs := Validate("notes/a.md", validDecayingFM(), true); len(probs) != 0 {
		t.Fatalf("valid note rejected: %v", probs)
	}
}

func TestNoFrontmatter(t *testing.T) {
	if probs := Validate("notes/a.md", nil, false); len(probs) == 0 {
		t.Fatal("missing frontmatter must be rejected")
	}
}

func TestRequiredCore(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(map[string]any)
		field string
	}{
		{"missing id", func(m map[string]any) { delete(m, "id") }, "id"},
		{"bad uuid", func(m map[string]any) { m["id"] = "not-a-uuid" }, "id"},
		{"missing category", func(m map[string]any) { delete(m, "category") }, "category"},
		{"unknown category", func(m map[string]any) { m["category"] = "vibes" }, "category"},
		{"missing created", func(m map[string]any) { delete(m, "created") }, "created"},
		{"bad created", func(m map[string]any) { m["created"] = "yesterday" }, "created"},
		{"missing updated", func(m map[string]any) { delete(m, "updated") }, "updated"},
	}
	for _, c := range cases {
		fm := validDecayingFM()
		c.mut(fm)
		assertField(t, Validate("notes/a.md", fm, true), c.field)
	}
}

func TestDecayingContract(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(map[string]any)
		field string
	}{
		{"missing confidence", func(m map[string]any) { delete(m, "confidence") }, "confidence"},
		{"confidence out of range", func(m map[string]any) { m["confidence"] = 1.5 }, "confidence"},
		{"confidence wrong type", func(m map[string]any) { m["confidence"] = "high" }, "confidence"},
		{"bad maturity", func(m map[string]any) { m["maturity"] = "ancient" }, "maturity"},
		{"missing last_reinforced", func(m map[string]any) { delete(m, "last_reinforced") }, "last_reinforced"},
		{"bad last_reinforced", func(m map[string]any) { m["last_reinforced"] = "soon" }, "last_reinforced"},
		{"missing reinforce_count", func(m map[string]any) { delete(m, "reinforce_count") }, "reinforce_count"},
		{"fractional reinforce_count", func(m map[string]any) { m["reinforce_count"] = 1.5 }, "reinforce_count"},
		{"empty sources", func(m map[string]any) { m["sources"] = []any{} }, "sources"},
		{"missing sources", func(m map[string]any) { delete(m, "sources") }, "sources"},
	}
	for _, c := range cases {
		fm := validDecayingFM()
		c.mut(fm)
		probs := Validate("notes/a.md", fm, true)
		if len(probs) == 0 {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		assertField(t, probs, c.field)
	}
}

func TestStatusRules(t *testing.T) {
	fm := validDecayingFM()
	fm["status"] = "bogus"
	assertField(t, Validate("notes/a.md", fm, true), "status")

	fm = validDecayingFM()
	fm["status"] = "falsified"
	probs := Validate("notes/a.md", fm, true)
	assertField(t, probs, "falsified_reason")
	assertField(t, probs, "falsified_at")

	fm["falsified_reason"] = "superseded by measurement"
	fm["falsified_at"] = "2026-07-12 11:00:00"
	if probs := Validate("notes/a.md", fm, true); len(probs) != 0 {
		t.Fatalf("complete falsified note rejected: %v", probs)
	}

	for _, s := range []string{"active", "disputed", "paused", "faded", "archived"} {
		fm := validDecayingFM()
		fm["status"] = s
		if probs := Validate("notes/a.md", fm, true); len(probs) != 0 {
			t.Errorf("status %q rejected: %v", s, probs)
		}
	}
}

func TestEntriesStage(t *testing.T) {
	fm := map[string]any{
		"id":       uuid.Must(uuid.NewV7()).String(),
		"category": "entry",
		"created":  "2026-07-12 09:00:00",
		"updated":  "2026-07-12 09:00:00",
	}
	if probs := Validate("entries/2026-07-12.md", fm, true); len(probs) != 0 {
		t.Fatalf("valid entry rejected: %v", probs)
	}
	fm["category"] = "concept"
	assertField(t, Validate("entries/2026-07-12.md", fm, true), "category")
}

func TestReflectionsStage(t *testing.T) {
	fm := map[string]any{
		"id":          uuid.Must(uuid.NewV7()).String(),
		"category":    "reflection",
		"persona":     "deep-thoughts",
		"description": "Ported the frontmatter contract from silo-kb.",
		"created":     "2026-07-12 12:00:00",
		"updated":     "2026-07-12 12:00:00",
	}
	if probs := Validate("reflections/2026-07-12-port.md", fm, true); len(probs) != 0 {
		t.Fatalf("valid reflection rejected: %v", probs)
	}

	noPersona := map[string]any{}
	for k, v := range fm {
		noPersona[k] = v
	}
	delete(noPersona, "persona")
	assertField(t, Validate("reflections/x.md", noPersona, true), "persona")

	noDesc := map[string]any{}
	for k, v := range fm {
		noDesc[k] = v
	}
	noDesc["description"] = "   "
	assertField(t, Validate("reflections/x.md", noDesc, true), "description")

	// persona is reflection-only
	stray := validDecayingFM()
	stray["persona"] = "deep-thoughts"
	assertField(t, Validate("notes/a.md", stray, true), "persona")
}

// TestArchiveExempt — archived notes keep whatever fields they faded with;
// only the universal core applies.
func TestArchiveExempt(t *testing.T) {
	fm := map[string]any{
		"id":       uuid.Must(uuid.NewV7()).String(),
		"category": "concept",
		"created":  "2026-01-01 00:00:00",
		"updated":  "2026-06-01 00:00:00",
		// no decay fields at all — legal in archive/
	}
	if probs := Validate("archive/old.md", fm, true); len(probs) != 0 {
		t.Fatalf("archived note rejected: %v", probs)
	}
}

func TestReservedFiles(t *testing.T) {
	if probs := Validate("index.md", map[string]any{"okf_version": 1}, true); len(probs) != 0 {
		t.Fatalf("root index.md rejected: %v", probs)
	}
	if probs := Validate("index.md", map[string]any{"okf_version": 1, "extra": true}, true); len(probs) == 0 {
		t.Fatal("root index.md with extra fields must be rejected")
	}
	if probs := Validate("log.md", map[string]any{"id": "x"}, true); len(probs) == 0 {
		t.Fatal("log.md with frontmatter must be rejected")
	}
	if probs := Validate("log.md", nil, false); len(probs) != 0 {
		t.Fatalf("bare log.md rejected: %v", probs)
	}
}

// TestProblemsNameField — a rejection names the offending field in a form the
// write path can surface.
func TestProblemsNameField(t *testing.T) {
	fm := validDecayingFM()
	delete(fm, "confidence")
	probs := Validate("notes/a.md", fm, true)
	if len(probs) == 0 {
		t.Fatal("expected a problem")
	}
	if !strings.Contains(probs[0].String(), "confidence") {
		t.Fatalf("problem string %q does not name the field", probs[0].String())
	}
}

func TestLinks(t *testing.T) {
	n := &Note{
		Frontmatter: map[string]any{
			"sources": []any{"[[2026-07-12]]", "[[2026-07-12]]", "  [[capture#section]]  "},
		},
		Body: "See [[rrf-fusion|the fusion note]] and [[rrf-fusion]] again, plus [[other]].",
	}
	refs := Targets(n)
	want := []Ref{
		{Name: "2026-07-12", Kind: Source},
		{Name: "capture", Kind: Source},
		{Name: "rrf-fusion", Kind: Wikilink},
		{Name: "other", Kind: Wikilink},
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("refs[%d] = %v, want %v", i, refs[i], want[i])
		}
	}
	srcs := Sources(n)
	if len(srcs) != 2 || srcs[0] != "2026-07-12" || srcs[1] != "capture" {
		t.Fatalf("sources = %v", srcs)
	}
}

// TestNoteIDMustBeV7 pins the id contract. Ids are written once and never
// rewritten, so an accepted version is permanent — this is the gate that makes
// "ids sort lexically by creation time" true rather than aspirational.
func TestNoteIDMustBeV7(t *testing.T) {
	cases := []struct {
		name    string
		id      any
		wantErr bool
	}{
		{"v7 accepted", uuid.Must(uuid.NewV7()).String(), false},
		{"v4 rejected", uuid.Must(uuid.NewRandom()).String(), true},
		{"v1 rejected", uuid.Must(uuid.NewUUID()).String(), true},
		{"nil uuid rejected", uuid.Nil.String(), true},
		{"not a uuid", "nope", true},
		{"missing", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm := validDecayingFM()
			fm["id"] = tc.id
			probs := Validate("notes/a.md", fm, true)
			var got bool
			for _, p := range probs {
				if p.Field == "id" {
					got = true
				}
			}
			if got != tc.wantErr {
				t.Errorf("id %v: problem reported = %v, want %v (problems: %v)",
					tc.id, got, tc.wantErr, fieldsOf(probs))
			}
		})
	}
}

// A rejected id must say which version it got, or the agent cannot tell a
// version problem from a malformed-string problem and will retry identically.
func TestV4RejectionNamesTheVersion(t *testing.T) {
	fm := validDecayingFM()
	fm["id"] = uuid.Must(uuid.NewRandom()).String()
	for _, p := range Validate("notes/a.md", fm, true) {
		if p.Field == "id" {
			if !strings.Contains(p.Reason, "v4") || !strings.Contains(p.Reason, "UUIDv7") {
				t.Errorf("reason %q should name both the required version and what it got", p.Reason)
			}
			return
		}
	}
	t.Fatal("no id problem reported for a v4 id")
}
