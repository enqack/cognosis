package query_test

import (
	"context"
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
	plan, err := s2.ExplainRankVector(ctx, table, []float32{1, 0, 0, 0, 0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll("testdata", 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join("testdata", "explain_vector_leg.txt")
	if err := os.WriteFile(artifact, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "hnsw_idx") {
		t.Fatalf("vector leg does not use the HNSW index; plan (recorded at %s):\n%s", artifact, plan)
	}
}
