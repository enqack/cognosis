package query_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

const (
	queryText  = "where is the index stored"
	pgBody     = "Postgres stores the derived index. See [[garden]] for soil."
	vaultBody  = "The vault reconciles hand edits."
	moonBody   = "The index is stored stored stored on the moon."
	gardenBody = "Completely unrelated gardening content."
)

// fixture builds the deterministic corpus: pinned stub vectors, one note
// reachable only through the link graph (garden — never embedded, no keyword
// overlap), and one falsified note (moon).
func fixture(t *testing.T) (*query.Engine, string, context.Context) {
	t.Helper()
	s, dsn := storetest.New(t)
	ctx := context.Background()

	stub := embedtest.New()
	stub.Vectors = map[string][]float32{
		queryText:  {1, 0, 0, 0, 0, 0, 0, 0},
		pgBody:     {1, 0, 0, 0, 0, 0, 0, 0},
		vaultBody:  {0.5, 0.866, 0, 0, 0, 0, 0, 0},
		moonBody:   {0.9, 0.436, 0, 0, 0, 0, 0, 0},
		gardenBody: {0, 0, 0, 0, 0, 0, 0, 1},
	}
	table := embed.TableSlug(stub.Name(), stub.Model())
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(), table, stub.Dim, true); err != nil {
		t.Fatal(err)
	}

	ix := &write.Indexer{Store: s, Provider: stub, Table: table}
	bare := &write.Indexer{Store: s} // no embeddings — graph-only reachability

	// garden first so pg's [[garden]] wikilink resolves.
	index(t, ctx, bare, "entries/garden.md", "", gardenBody)
	index(t, ctx, ix, "entries/pg.md", "", pgBody)
	index(t, ctx, ix, "entries/vault.md", "", vaultBody)
	index(t, ctx, ix, "entries/moon.md", "falsified", moonBody)
	indexConcept(t, ctx, ix, "notes/scoped.md", "alpha", "Project-scoped capture about the index.")

	e := &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: stub, Table: table}}}
	return e, dsn, ctx
}

func index(t *testing.T, ctx context.Context, ix *write.Indexer, rel, status, body string) {
	t.Helper()
	fm := "---\nid: " + uuid.NewString() + "\ncategory: entry\n" +
		"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n"
	if status != "" {
		fm += "status: " + status + "\n"
	}
	indexRaw(t, ctx, ix, rel, fm+"---\n"+body+"\n")
}

// indexConcept lands a decaying concept note (notes/ stage) so the fixture
// has category diversity for the bias tests.
func indexConcept(t *testing.T, ctx context.Context, ix *write.Indexer, rel, project, body string) {
	t.Helper()
	fm := "---\nid: " + uuid.NewString() + "\ncategory: concept\n" +
		"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n" +
		"confidence: 0.5\nmaturity: seed\nlast_reinforced: \"2026-07-12 09:00:00\"\nreinforce_count: 0\n" +
		"sources:\n  - \"[[garden]]\"\n"
	if project != "" {
		fm += "project: " + project + "\n"
	}
	indexRaw(t, ctx, ix, rel, fm+"---\n"+body+"\n")
}

func indexRaw(t *testing.T, ctx context.Context, ix *write.Indexer, rel, content string) {
	t.Helper()
	n, err := vault.ParseNote(rel, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	meta := write.FileMeta{Mtime: time.Now(), Size: int64(len(content)), Blake3: rel}
	if err := ix.Index(ctx, n, meta); err != nil {
		t.Fatalf("index %s: %v", rel, err)
	}
}

func paths(rs []query.Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Path)
	}
	return out
}

// TestGoldenRanking — the fused default-query ordering matches the committed
// golden file exactly.
func TestGoldenRanking(t *testing.T) {
	e, _, ctx := fixture(t)
	rs, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(paths(rs), "\n") + "\n"

	goldenPath := filepath.Join("testdata", "golden_rankings.txt")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Fatalf("ranking drifted from golden:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

// TestGraphOnlyNoteAppears — garden has no embedding and no keyword overlap;
// only the link graph can reach it, and fusion must surface it.
func TestGraphOnlyNoteAppears(t *testing.T) {
	e, _, ctx := fixture(t)
	rs, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths(rs) {
		if p == "entries/garden.md" {
			return
		}
	}
	t.Fatalf("graph-only note missing from fused results: %v", paths(rs))
}

func TestFalsifiedFiltered(t *testing.T) {
	e, _, ctx := fixture(t)

	rs, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths(rs) {
		if p == "entries/moon.md" {
			t.Fatal("falsified note leaked into default results")
		}
	}

	rs, err = e.Run(ctx, queryText, query.Options{IncludeFalsified: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range paths(rs) {
		if p == "entries/moon.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("include_falsified did not surface the falsified note: %v", paths(rs))
	}
}

func TestProjectScoping(t *testing.T) {
	e, _, ctx := fixture(t)
	rs, err := e.Run(ctx, queryText, query.Options{Project: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Path != "notes/scoped.md" && !strings.Contains(r.Path, "garden") {
			t.Fatalf("out-of-project result %q leaked through scoping", r.Path)
		}
	}
}

func TestTopK(t *testing.T) {
	e, _, ctx := fixture(t)
	rs, err := e.Run(ctx, queryText, query.Options{TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("topK=1 returned %d", len(rs))
	}
}

// TestCategoryBiasReorders — a persona's retrieval lens measurably reorders
// the fixture corpus versus the unbiased run (golden files for both), without
// excluding anything.
func TestCategoryBiasReorders(t *testing.T) {
	e, _, ctx := fixture(t)

	unbiased, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A concept-favoring lens (e.g. a theory-minded persona) lifts the
	// concept note from third to first without excluding anything.
	biased, err := e.Run(ctx, queryText, query.Options{
		CategoryBias: map[string]float64{"concept": 3.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same membership — a bias never excludes.
	if len(biased) != len(unbiased) {
		t.Fatalf("bias changed membership: %d vs %d", len(biased), len(unbiased))
	}

	goldenUnbiased := filepath.Join("testdata", "golden_rankings.txt")
	want, err := os.ReadFile(goldenUnbiased)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(unbiased), "\n") + "\n"; got != string(want) {
		t.Fatalf("unbiased ranking drifted:\n%s", got)
	}
	goldenBiased := filepath.Join("testdata", "golden_rankings_biased.txt")
	wantB, err := os.ReadFile(goldenBiased)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(biased), "\n") + "\n"; got != string(wantB) {
		t.Fatalf("biased ranking drifted from golden:\n--- got ---\n%s--- want ---\n%s", got, wantB)
	}
}

// TestExplainVectorLegUsesHNSW — the planner's strategy for the vector leg is
// recorded as a test artifact and must show the HNSW index (seq scans are
// disabled for the check since the planner would prefer them on a tiny
// fixture corpus).
func TestExplainVectorLegUsesHNSW(t *testing.T) {
	_, dsn, ctx := fixture(t)

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("options", q.Get("options")+" -cenable_seqscan=off")
	u.RawQuery = q.Encode()

	s2, err := store.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	table := embed.TableSlug("stub", "stub-model")
	vec := []float32{1, 0, 0, 0, 0, 0, 0, 0}

	// Record the production leg shape under both an unscoped and a
	// project-scoped filter. The filter matters: it is what pulls the notes
	// join and the status predicates into the plan, and those are exactly what
	// an earlier stripped version of this query omitted.
	var b strings.Builder
	b.WriteString("EXPLAIN of the production vector leg (store.vectorLegSQL), recorded per scope.\n\n" +
		"enable_seqscan is forced off: the 4-note fixture is far below the ~3k-chunk\n" +
		"threshold at which the planner reaches for HNSW unaided, so without the flag\n" +
		"this would record a seqscan and prove nothing about index usage.\n\n" +
		"What the two scopes show is the point of recording both. Unscoped, the planner\n" +
		"takes the HNSW index and gets approximate neighbours. Scoped to a project it\n" +
		"declines HNSW — with the index still available — and instead drives from\n" +
		"notes_project_idx through chunks to the embeddings pkey, sorting exactly. That\n" +
		"is the access-path switch by scope selectivity, and the previous version of this\n" +
		"artifact could not show it: it explained a query with no notes join and no WHERE.\n\n" +
		"Magnitudes (how much recall the approximate path loses, and where) are measured\n" +
		"in internal/query/retrievaleval on a corpus large enough for the question to be\n" +
		"real. This artifact is the cheap structural check that the shapes are right.\n")

	for _, sc := range []struct {
		name string
		// wantHNSW is the expected access path. Scoped queries legitimately
		// do NOT use HNSW — the planner pre-filters and sorts exactly, which
		// is better, not worse. Asserting HNSW everywhere would be asserting
		// a defect.
		wantHNSW bool
		mustHave string
		filter   store.Filter
	}{
		{"unscoped", true, "hnsw_idx", store.Filter{}},
		{"project=alpha", false, "project = 'alpha'", store.Filter{Project: "alpha"}},
	} {
		plan, err := s2.ExplainRankVector(ctx, table, vec, sc.filter, 10)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "\n=== scope: %s ===\n%s", sc.name, plan)

		if got := strings.Contains(plan, "hnsw_idx"); got != sc.wantHNSW {
			t.Errorf("scope %s: HNSW used = %v, want %v; plan:\n%s", sc.name, got, sc.wantHNSW, plan)
		}
		if !strings.Contains(plan, sc.mustHave) {
			t.Errorf("scope %s: plan is missing %q — the filter did not reach the plan:\n%s",
				sc.name, sc.mustHave, plan)
		}
		// The production leg joins notes; a plan without it is the stripped
		// query this test used to explain, which could say nothing about
		// filtered scans.
		if !strings.Contains(plan, "notes") {
			t.Errorf("scope %s: plan has no notes relation — this is not the production leg:\n%s",
				sc.name, plan)
		}
	}

	if err := os.MkdirAll("testdata", 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join("testdata", "explain_vector_leg.txt")
	if err := os.WriteFile(artifact, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %s", artifact)
}

// TestCountSuppressedFalsified backs the retrieval hint that tells an agent
// suppressed history exists. Without it, falsified notes are filtered in SQL
// and the caller sees nothing at all — so an agent in an unusual context
// silently reinvents what the vault already ruled out.
func TestCountSuppressedFalsified(t *testing.T) {
	_, dsn, ctx := fixture(t)
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// moon.md is the fixture's falsified note; its body is moonBody.
	n, err := s.CountSuppressedFalsified(ctx, "moon", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (the falsified moon note matches 'moon')", n)
	}

	// Nothing is being suppressed when the caller already asked for them —
	// reporting a count there would tell the agent to do what it just did.
	n, err = s.CountSuppressedFalsified(ctx, "moon", store.Filter{IncludeFalsified: true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count = %d with include_falsified, want 0", n)
	}

	// A query matching only live notes must not claim suppressed history.
	n, err = s.CountSuppressedFalsified(ctx, "gardening", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count = %d for a query no falsified note matches, want 0", n)
	}
}

// TestRunWithStatsCountsEachLeg — the counts exist to drive a decision about
// the keyword leg, so a plausible-but-wrong number is worse than none. This
// pins them against the legs the engine actually ran rather than against each
// other.
//
// The specific failure it guards: FTS is written from inside the errgroup
// alongside the vector legs, so a count assigned to the wrong slot, or lost to
// the race between them, would still produce a believable log line.
func TestRunWithStatsCountsEachLeg(t *testing.T) {
	e, _, ctx := fixture(t)

	results, stats, err := e.RunWithStats(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fused < len(results) {
		t.Errorf("fused=%d is below the %d results returned; the fused count is taken before "+
			"the top-K cut and cannot be smaller", stats.Fused, len(results))
	}
	if stats.Vector == 0 && stats.FTS == 0 && stats.Graph == 0 {
		t.Fatal("every leg reported zero candidates yet the query returned results")
	}
	if stats.Fused == 0 {
		t.Error("fused count is zero for a query that returned results")
	}

	// Each leg is counted independently: disabling the graph leg must zero
	// that count and leave the others intact. Asserting only on totals would
	// pass if two legs' counts were swapped.
	graphOn := stats
	e.Tuning = query.Tuning{DisableGraph: true}
	if _, off, err := e.RunWithStats(ctx, queryText, query.Options{}); err != nil {
		t.Fatal(err)
	} else {
		if off.Graph != 0 {
			t.Errorf("graph=%d with DisableGraph set", off.Graph)
		}
		if off.Vector != graphOn.Vector || off.FTS != graphOn.FTS {
			t.Errorf("disabling the graph leg changed the other counts: vector %d->%d, fts %d->%d",
				graphOn.Vector, off.Vector, graphOn.FTS, off.FTS)
		}
	}
}

// Run must stay a thin delegation to RunWithStats — one implementation, so the
// counts describe the query that actually ran rather than a parallel copy of
// the logic that could drift from it.
func TestRunMatchesRunWithStats(t *testing.T) {
	e, _, ctx := fixture(t)

	plain, err := e.Run(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	withStats, _, err := e.RunWithStats(ctx, queryText, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != len(withStats) {
		t.Fatalf("Run returned %d results, RunWithStats %d", len(plain), len(withStats))
	}
	for i := range plain {
		if plain[i].Path != withStats[i].Path {
			t.Errorf("rank %d: Run=%s RunWithStats=%s", i, plain[i].Path, withStats[i].Path)
		}
	}
}
