package store

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

// paths extracts note paths from a leg's ranked chunks, in order.
func paths(cs []RankedChunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.NotePath
	}
	return out
}

// TestRankGraphMultiHop pins the two load-bearing properties of the multi-hop
// graph leg: at maxDepth=1 it is byte-for-byte the shipped one-hop leg, and at
// maxDepth=2 it surfaces the hop-2 notes one hop cannot reach, discounted below
// the hop-1 notes. Fixture:
//
//	S1 -> A,  S2 -> A,  S1 -> B      (hop 1: A reached by 2 seeds, B by 1)
//	A -> C,   B -> D                 (hop 2: C, D, reachable only via A/B)
//
// seeds = {S1, S2}.
func TestRankGraphMultiHop(t *testing.T) {
	s, ctx := testStore(t)

	mk := func(path string) Note {
		n := testNote(path)
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
		// The graph leg joins chunks; every candidate note needs at least one.
		if err := s.ReplaceChunks(ctx, n.Path, []Chunk{
			{Ordinal: 0, Content: "body of " + path, ContentHash: path},
		}); err != nil {
			t.Fatalf("chunks %s: %v", path, err)
		}
		return n
	}

	s1, s2 := mk("notes/s1.md"), mk("notes/s2.md")
	a, b := mk("notes/a.md"), mk("notes/b.md")
	c, d := mk("notes/c.md"), mk("notes/d.md")

	link := func(src Note, dsts ...Note) {
		ls := make([]Link, len(dsts))
		for i, dst := range dsts {
			ls[i] = Link{Dst: dst.ID, Kind: "wikilink"}
		}
		if err := s.SetLinks(ctx, src.ID, ls); err != nil {
			t.Fatalf("links from %s: %v", src.Path, err)
		}
	}
	link(s1, a, b)
	link(s2, a)
	link(a, c)
	link(b, d)

	seeds := []uuid.UUID{s1.ID, s2.ID}
	f := Filter{}

	// Property 1: maxDepth=1 reproduces the shipped one-hop leg exactly.
	shipped, err := s.RankGraph(ctx, seeds, f, 50)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := s.RankGraphMulti(ctx, seeds, 1, 0.5, f, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths(d1), paths(shipped); !equalStrings(got, want) {
		t.Fatalf("depth-1 diverges from shipped leg:\n multi=%v\n shipped=%v", got, want)
	}
	// Hop-1 membership: A and B only, A first (reached by 2 seeds vs 1).
	if want := []string{"notes/a.md", "notes/b.md"}; !equalStrings(paths(d1), want) {
		t.Fatalf("depth-1 membership/order = %v, want %v", paths(d1), want)
	}

	// Property 2: maxDepth=2 adds the hop-2 notes (C, D), ranked below hop-1.
	d2, err := s.RankGraphMulti(ctx, seeds, 2, 0.5, f, 50)
	if err != nil {
		t.Fatal(err)
	}
	set := func(ps []string) map[string]bool {
		m := map[string]bool{}
		for _, p := range ps {
			m[p] = true
		}
		return m
	}
	d1set, d2set := set(paths(d1)), set(paths(d2))
	for _, p := range []string{"notes/c.md", "notes/d.md"} {
		if d1set[p] {
			t.Fatalf("%s should be unreachable at depth 1", p)
		}
		if !d2set[p] {
			t.Fatalf("%s should be surfaced at depth 2", p)
		}
	}
	// Distance decay: the two hop-1 notes outrank both hop-2 notes.
	if got := paths(d2)[:2]; !equalStrings(got, []string{"notes/a.md", "notes/b.md"}) {
		t.Fatalf("depth-2 top-2 = %v, want the hop-1 notes [a, b]", got)
	}
}

// TestRankGraphMultiShelvedNoConduct pins the soft-delete/temporal invariant for
// depth>=2: activation must not route THROUGH a shelved (archived/falsified) note
// to reach a live one, and the IncludeArchived/IncludeFalsified flags re-enable
// conduction exactly when the caller has asked to see those notes. Fixture:
//
//	S -> Aarch(archived)   -> La(live)
//	S -> Afals(falsified)  -> Lf(live)
//
// seeds = {S}. La is reachable ONLY through Aarch; Lf ONLY through Afals.
func TestRankGraphMultiShelvedNoConduct(t *testing.T) {
	s, ctx := testStore(t)

	mk := func(path, status string) Note {
		n := testNote(path)
		n.Status = status
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
		if err := s.ReplaceChunks(ctx, n.Path, []Chunk{
			{Ordinal: 0, Content: "body of " + path, ContentHash: path},
		}); err != nil {
			t.Fatalf("chunks %s: %v", path, err)
		}
		return n
	}
	// SetLinks REPLACES a note's whole outbound set, so every source's links must
	// be set in one call.
	link := func(src Note, dsts ...Note) {
		ls := make([]Link, len(dsts))
		for i, dst := range dsts {
			ls[i] = Link{Dst: dst.ID, Kind: "wikilink"}
		}
		if err := s.SetLinks(ctx, src.ID, ls); err != nil {
			t.Fatalf("links from %s: %v", src.Path, err)
		}
	}

	sNote := mk("notes/seed.md", "active")
	aArch := mk("notes/arch.md", "archived")
	la := mk("notes/la.md", "active")
	aFals := mk("notes/fals.md", "falsified")
	lf := mk("notes/lf.md", "active")
	link(sNote, aArch, aFals)
	link(aArch, la)
	link(aFals, lf)

	seeds := []uuid.UUID{sNote.ID}
	has := func(cs []RankedChunk, path string) bool {
		return slices.Contains(paths(cs), path)
	}

	// Default filter: neither shelved note conducts, so neither live note behind
	// one is reachable at depth 2 (and the shelved notes themselves are excluded).
	def, err := s.RankGraphMulti(ctx, seeds, 2, 0.5, Filter{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"notes/la.md", "notes/lf.md", "notes/arch.md", "notes/fals.md"} {
		if has(def, p) {
			t.Fatalf("default filter: %s must not appear (reached only via a shelved bridge)", p)
		}
	}

	// IncludeArchived: the archived bridge conducts -> La reachable; the falsified
	// bridge still does not -> Lf unreachable.
	arch, err := s.RankGraphMulti(ctx, seeds, 2, 0.5, Filter{IncludeArchived: true}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !has(arch, "notes/la.md") {
		t.Fatal("IncludeArchived: La should be reachable through the now-visible archived bridge")
	}
	if has(arch, "notes/lf.md") {
		t.Fatal("IncludeArchived: Lf must stay unreachable (its bridge is falsified, not archived)")
	}

	// IncludeFalsified: mirror image -- the falsified bridge conducts, archived does not.
	fals, err := s.RankGraphMulti(ctx, seeds, 2, 0.5, Filter{IncludeFalsified: true}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !has(fals, "notes/lf.md") {
		t.Fatal("IncludeFalsified: Lf should be reachable through the now-visible falsified bridge")
	}
	if has(fals, "notes/la.md") {
		t.Fatal("IncludeFalsified: La must stay unreachable (its bridge is archived, not falsified)")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
