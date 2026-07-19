package query_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

const temporalQuery = "chronicle of the index design"

// temporalFixture: three notes sharing enough keyword/vector signal that all
// would rank, with synthetic timestamps that as_of reasons over:
//   - old.md      created 2026-01-01, live
//   - newer.md    created 2026-06-01, live
//   - flipped.md  created 2026-01-01, falsified at 2026-05-01
func temporalFixture(t *testing.T) (*query.Engine, context.Context) {
	t.Helper()
	s, _ := storetest.New(t)
	ctx := context.Background()

	stub := embedtest.New()
	stub.Vectors = map[string][]float32{
		temporalQuery: {1, 0, 0, 0, 0, 0, 0, 0},
		"The original chronicle of the index design.":                {1, 0, 0, 0, 0, 0, 0, 0},
		"A newer chronicle of the index design.":                     {0.9, 0.436, 0, 0, 0, 0, 0, 0},
		"A disproven chronicle of the index design, kept as record.": {0.8, 0.6, 0, 0, 0, 0, 0, 0},
	}
	table := embed.TableSlug(stub.Name(), stub.Model())
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(), table, stub.Dim, true); err != nil {
		t.Fatal(err)
	}
	ix := &write.Indexer{Store: s, Provider: stub, Table: table}

	put := func(rel, created, body string, extraFM string) {
		t.Helper()
		content := "---\nid: " + uuid.NewString() + "\ncategory: entry\n" +
			"created: \"" + created + "\"\nupdated: \"" + created + "\"\n" + extraFM +
			"---\n" + body + "\n"
		n, err := vault.ParseNote(rel, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if err := ix.Index(ctx, n, write.FileMeta{Mtime: time.Now(), Size: 1, Blake3: rel}); err != nil {
			t.Fatal(err)
		}
	}

	put("entries/old.md", "2026-01-01 10:00:00", "The original chronicle of the index design.", "")
	put("entries/newer.md", "2026-06-01 10:00:00", "A newer chronicle of the index design.", "")
	put("entries/flipped.md", "2026-01-01 12:00:00", "A disproven chronicle of the index design, kept as record.",
		"status: falsified\nfalsified_reason: measured otherwise\nfalsified_at: \"2026-05-01 00:00:00\"\n")

	return &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: stub, Table: table}}}, ctx
}

func asOf(t *testing.T, s string) *time.Time {
	t.Helper()
	tm, err := vault.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return &tm
}

func has(rs []query.Result, path string) bool {
	for _, r := range rs {
		if r.Path == path {
			return true
		}
	}
	return false
}

// TestAsOfEarly -- at T before the falsification and before newer's creation:
// the KB believed old AND flipped; newer didn't exist yet.
func TestAsOfEarly(t *testing.T) {
	e, ctx := temporalFixture(t)
	rs, err := e.Run(ctx, temporalQuery, query.Options{AsOf: asOf(t, "2026-03-01 00:00:00")})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(rs)
	want := []string{"entries/old.md", "entries/flipped.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("as_of early ranking = %v, want %v", got, want)
	}
}

// TestAsOfLate -- at T after both events: newer exists, flipped is no longer
// believed.
func TestAsOfLate(t *testing.T) {
	e, ctx := temporalFixture(t)
	rs, err := e.Run(ctx, temporalQuery, query.Options{AsOf: asOf(t, "2026-06-15 00:00:00")})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(rs)
	want := []string{"entries/old.md", "entries/newer.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("as_of late ranking = %v, want %v", got, want)
	}
}

// TestAsOfOmittedUnchanged -- no as_of keeps current behavior: falsified
// excluded, everything current included.
func TestAsOfOmittedUnchanged(t *testing.T) {
	e, ctx := temporalFixture(t)
	rs, err := e.Run(ctx, temporalQuery, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !has(rs, "entries/old.md") || !has(rs, "entries/newer.md") {
		t.Fatalf("current view missing live notes: %v", paths(rs))
	}
	if has(rs, "entries/flipped.md") {
		t.Fatalf("falsified note leaked into current view: %v", paths(rs))
	}
}

// TestAsOfWithIncludeFalsified -- include_falsified still overrides at T.
func TestAsOfWithIncludeFalsified(t *testing.T) {
	e, ctx := temporalFixture(t)
	rs, err := e.Run(ctx, temporalQuery, query.Options{
		AsOf: asOf(t, "2026-06-15 00:00:00"), IncludeFalsified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !has(rs, "entries/flipped.md") {
		t.Fatalf("include_falsified under as_of did not surface the falsified note: %v", paths(rs))
	}
}

// TestListDecaying -- synthetic last_reinforced spread; shielded notes
// (paused, graduated, falsified) never listed.
func TestListDecaying(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	old := "2026-01-01 00:00:00"
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Now().Format("2006-01-02 15:04:05")

	// created and lastExplicit are what the shortlist keys on now;
	// lastReinforced is the decay clock, which passive refresh and decay both
	// move and which therefore cannot answer "has anyone asserted this".
	put := func(rel string, created time.Time, lastReinforced, lastExplicit, extra string) {
		id := uuid.Must(uuid.NewV7())
		fm := map[string]any{
			"id": id.String(), "category": "concept",
			"last_reinforced": lastReinforced,
		}
		if lastExplicit != "" {
			fm["last_explicit_reinforce"] = lastExplicit
		}
		n := store.Note{
			Path: rel, ID: id, Category: "concept", Status: "active",
			Created: created, Updated: time.Now().UTC(),
			Frontmatter: fm, Content: "x", Mtime: time.Now().UTC(), Size: 1, Blake3: rel,
		}
		conf := 0.5
		n.Confidence = &conf
		mat := "seed"
		n.Maturity = &mat
		switch extra {
		case "paused":
			n.Status = "paused"
		case "falsified":
			n.Status = "falsified"
		case "graduated":
			fm["graduated_at"] = "2026-01-01 00:00:00"
		}
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	put("notes/stale.md", oldTime, old, "", "")       // never asserted, born long ago
	put("notes/fresh.md", now, fresh, "", "")         // born today
	put("notes/asserted.md", oldTime, old, fresh, "") // old note, asserted recently
	put("notes/carried.md", oldTime, fresh, old, "")  // clock fresh, assertion old
	put("notes/paused.md", oldTime, old, "", "paused")
	put("notes/dead.md", oldTime, old, "", "falsified")
	put("notes/canon.md", oldTime, old, "", "graduated")
	put("entries/not-a-theory.md", oldTime, old, "", "") // wrong stage

	cutoff := time.Now().AddDate(0, 0, -30)
	got, err := s.ListDecaying(ctx, cutoff, "")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(got))
	for _, d := range got {
		paths = append(paths, d.Path)
	}
	// carried.md is the point of this change: passive refresh (or decay) has
	// been moving its last_reinforced, so keying on that field would drop the
	// note from the very shortlist that exists to surface it.
	want := []string{"notes/stale.md", "notes/carried.md"}
	if len(paths) != len(want) {
		t.Fatalf("decaying = %v, want exactly %v", paths, want)
	}
	for _, w := range want {
		if !slices.Contains(paths, w) {
			t.Fatalf("decaying = %v, missing %s", paths, w)
		}
	}
	for _, d := range got {
		if d.Confidence != 0.5 || d.Maturity != "seed" {
			t.Fatalf("row = %+v", d)
		}
		if d.LastAsserted == "" {
			t.Errorf("%s: LastAsserted empty; the shortlist cannot show what it ordered by", d.Path)
		}
	}
}
