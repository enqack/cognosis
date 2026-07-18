package retrievaleval

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

// CorpusSpec describes a synthetic vault to measure retrieval against.
type CorpusSpec struct {
	Notes         int
	ChunksPerNote int

	// ProjectMix assigns notes to projects by share; keys are project names
	// ("" is the unscoped default project) and values need not sum to 1 (they
	// are normalized). Skew is load-bearing, not decoration: round-robin
	// assignment gives uniform selectivity, and it is the *selective* scope
	// that forces a filtered scan to walk far past its candidate list.
	ProjectMix map[string]float64

	FalsifiedFrac float64 // share of notes with status=falsified
	ArchivedFrac  float64 // share of notes with status=archived
	LinkDegree    int     // outbound wikilinks per note (graph leg fuel)
	// ArchivedLinkFrac is the share of notes given an outbound link to an
	// archived note, which is what triggers query.archivedLinkPenalty.
	ArchivedLinkFrac float64

	Dim      int
	Clusters int
	Spread   float64
	Seed     int64

	// Queries is how many evaluation queries to generate. Recall must be
	// averaged over many queries: HNSW is not monotone per-query, so a
	// single-query measurement is noise.
	Queries int
}

// DefaultSpec is sized from the Phase 0 finding that the planner chooses a
// seqscan below ~5k chunks — measuring ANN recall on a corpus the planner
// refuses to use an index for silently reports perfect recall.
func DefaultSpec() CorpusSpec {
	return CorpusSpec{
		Notes:            4000,
		ChunksPerNote:    5, // 20k chunks
		ProjectMix:       map[string]float64{"": 0.74, "wide": 0.25, "narrow": 0.01},
		FalsifiedFrac:    0.04,
		ArchivedFrac:     0.08,
		LinkDegree:       3,
		ArchivedLinkFrac: 0.10,
		Dim:              768,
		Clusters:         40,
		Spread:           DefaultSpread,
		Seed:             7,
		Queries:          30,
	}
}

// EvalQuery is one evaluation probe with its pseudo-relevance label.
type EvalQuery struct {
	Text    string
	Cluster int
}

// Corpus is a seeded synthetic vault plus everything needed to query it.
type Corpus struct {
	Store    *store.Store
	DSN      string
	Table    string
	Provider *Synth
	Engine   *query.Engine
	Queries  []EvalQuery
	Spec     CorpusSpec

	// InScope counts live (non-falsified, non-archived) chunks per project
	// key, so tests can distinguish "the scan truncated" from "the scope only
	// holds this many rows" — a distinction Phase 0 got wrong once already.
	InScope map[string]int
}

// clusterVocab returns the deterministic word bag for a cluster. Per-cluster
// vocabularies make cluster membership simultaneously the vector ground truth
// and a keyword ground truth, which is what lets the FTS leg be scored at all.
func clusterVocab(cluster, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("lex%03dw%02d", cluster, i)
	}
	return out
}

// Build seeds the corpus and returns a wired Engine. Skips when
// COGNOSIS_TEST_DSN is unset.
func Build(t testing.TB, spec CorpusSpec) *Corpus {
	t.Helper()
	ctx := context.Background()
	s, dsn := storetest.NewTB(t)

	syn := NewSynth(spec.Dim, spec.Clusters, spec.Seed, spec.Spread)
	syn.Labels = map[string]int{}
	table := embed.TableSlug(syn.Name(), syn.Model())
	if err := s.EnsureProvider(ctx, syn.Name(), syn.Model(), table, spec.Dim, true); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}

	rng := rand.New(rand.NewSource(spec.Seed)) //nolint:gosec // reproducibility, not secrecy
	projects := weightedProjects(spec.ProjectMix)
	now := time.Now().UTC().Truncate(time.Second)

	type noteRec struct {
		id      uuid.UUID
		path    string
		project string
		status  string
	}
	notes := make([]noteRec, spec.Notes)
	var archived []uuid.UUID
	inScope := map[string]int{}

	// Pass 1: notes and chunks.
	//
	// Project and status are drawn independently. They must not be derived
	// from colliding moduli of the same index: a first attempt at this used
	// i%97 for project and i%25 for status, which made every note in the
	// smallest project also archived, leaving the selective scope empty.
	allVecs := make([][]float32, 0, spec.Notes*spec.ChunksPerNote)
	for i := range notes {
		id := uuid.New()
		path := fmt.Sprintf("notes/n%05d.md", i)
		project := projects[rng.Intn(len(projects))]

		status := "active"
		switch r := rng.Float64(); {
		case r < spec.FalsifiedFrac:
			status = "falsified"
		case r < spec.FalsifiedFrac+spec.ArchivedFrac:
			status = "archived"
		}
		notes[i] = noteRec{id, path, project, status}
		if status == "archived" {
			archived = append(archived, id)
		}

		cluster := rng.Intn(spec.Clusters)
		vocab := clusterVocab(cluster, 12)

		chunks := make([]store.Chunk, spec.ChunksPerNote)
		vecs := map[uuid.UUID][]float32{}
		texts := make([]string, spec.ChunksPerNote)
		for o := range chunks {
			// Draw a deterministic subset of the cluster's vocabulary.
			var b strings.Builder
			fmt.Fprintf(&b, "note %05d section %d about ", i, o)
			for range 8 {
				b.WriteString(vocab[rng.Intn(len(vocab))])
				b.WriteByte(' ')
			}
			text := strings.TrimSpace(b.String())
			texts[o] = text
			// Pin the label so geometry, label, and vocabulary agree.
			syn.Labels[text] = cluster
			chunks[o] = store.Chunk{
				Ordinal:     o,
				HeadingPath: fmt.Sprintf("section %d", o),
				Content:     text,
				ContentHash: fmt.Sprintf("%x", i*1000+o),
			}
		}

		fm := map[string]any{"id": id.String(), "category": "concept"}
		if status == "falsified" {
			fm["falsified_at"] = now.Format("2006-01-02 15:04:05")
		}
		if status == "archived" {
			fm["archived_at"] = now.Format("2006-01-02 15:04:05")
		}
		n := store.Note{
			Path: path, ID: id, Project: project, Category: "concept",
			Status: status, Created: now.Add(-time.Duration(i) * time.Minute),
			Updated:     now.Add(-time.Duration(i) * time.Minute),
			Frontmatter: fm, Content: strings.Join(texts, "\n\n"),
			Summary: fmt.Sprintf("summary of note %05d", i),
			Mtime:   now, Size: 1, Blake3: "x",
		}
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatalf("upsert note %d: %v", i, err)
		}
		if err := s.ReplaceChunks(ctx, path, chunks); err != nil {
			t.Fatalf("replace chunks %d: %v", i, err)
		}

		// Embed and record.
		embedded, err := syn.Embed(ctx, texts)
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		ids, err := chunkIDs(ctx, s, path)
		if err != nil {
			t.Fatalf("chunk ids %d: %v", i, err)
		}
		for o, cid := range ids {
			vecs[cid] = embedded[o]
			allVecs = append(allVecs, embedded[o])
		}
		if err := s.UpsertEmbeddings(ctx, table, vecs); err != nil {
			t.Fatalf("upsert embeddings %d: %v", i, err)
		}

		if status == "active" {
			inScope[project] += spec.ChunksPerNote
			inScope[""] += spec.ChunksPerNote // "" scope matches everything
		}
	}

	// The guard Phase 0 earned: a corpus of identical vectors produces
	// confident, plausible, meaningless numbers.
	if d := DistinctVectors(allVecs); d != len(allVecs) {
		t.Fatalf("degenerate corpus: %d distinct vectors from %d chunks", d, len(allVecs))
	}

	// Pass 2: links, now that every note id exists.
	for i, n := range notes {
		var links []store.Link
		for range spec.LinkDegree {
			tgt := notes[rng.Intn(len(notes))]
			if tgt.id != n.id {
				links = append(links, store.Link{Dst: tgt.id, Kind: "wikilink"})
			}
		}
		if len(archived) > 0 && rng.Float64() < spec.ArchivedLinkFrac {
			links = append(links, store.Link{Dst: archived[rng.Intn(len(archived))], Kind: "wikilink"})
		}
		if len(links) > 0 {
			if err := s.SetLinks(ctx, n.id, links); err != nil {
				t.Fatalf("set links %d: %v", i, err)
			}
		}
	}

	// ANALYZE is not optional. Without it the planner works from its default
	// estimate (1070 rows, whatever the table actually holds) and its access-
	// path choices are neither production-like nor stable — a freshly seeded
	// 360-chunk corpus chose an HNSW scan on default stats where an analyzed
	// one of the same size chooses a seqscan. A real database is autovacuum-
	// analyzed; measuring against un-analyzed stats measures nothing anyone
	// runs.
	analyzeCorpus(ctx, t, dsn, table)

	// Evaluation queries: one per cluster, drawn from that cluster's
	// vocabulary so both the vector and keyword legs have a real signal.
	queries := make([]EvalQuery, spec.Queries)
	for q := range queries {
		cluster := q % spec.Clusters
		vocab := clusterVocab(cluster, 12)
		var b strings.Builder
		for w := range 5 {
			b.WriteString(vocab[(q+w)%len(vocab)])
			b.WriteByte(' ')
		}
		text := strings.TrimSpace(b.String())
		syn.Labels[text] = cluster
		queries[q] = EvalQuery{Text: text, Cluster: cluster}
	}

	return &Corpus{
		Store: s, DSN: dsn, Table: table, Provider: syn,
		Engine: &query.Engine{
			Store:     s,
			Providers: []query.ProviderLeg{{Provider: syn, Table: table}},
		},
		Queries: queries, Spec: spec, InScope: inScope,
	}
}

// Scopes are the named filter scopes the sweeps run over.
func (c *Corpus) Scopes() map[string]store.Filter {
	out := map[string]store.Filter{
		"all": {},
	}
	for p := range c.Spec.ProjectMix {
		if p == "" {
			continue
		}
		out[p] = store.Filter{Project: p}
	}
	out["with_archived"] = store.Filter{IncludeArchived: true, IncludeFalsified: true}
	return out
}

// ScopeNames returns Scopes' keys in a stable order, so recorded artifacts
// don't churn on Go's randomized map iteration.
func (c *Corpus) ScopeNames() []string {
	names := make([]string, 0, len(c.Scopes()))
	for k := range c.Scopes() {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// analyzeCorpus refreshes planner statistics on the seeded tables. It opens a
// direct connection because ANALYZE is a maintenance command with no business
// on the Store API — nothing in the daemon ever needs it.
func analyzeCorpus(ctx context.Context, t testing.TB, dsn, table string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("analyze: connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, tbl := range []string{"notes", "chunks", "links", table} {
		if _, err := conn.Exec(ctx, "analyze "+pgx.Identifier{tbl}.Sanitize()); err != nil {
			t.Fatalf("analyze %s: %v", tbl, err)
		}
	}
}

// chunkIDs returns a note's chunk ids in ordinal order. ChunkRefsForNote
// already orders by ordinal, so this just projects out the ids.
func chunkIDs(ctx context.Context, s *store.Store, path string) ([]uuid.UUID, error) {
	refs, err := s.ChunkRefsForNote(ctx, path)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids, nil
}

// weightedProjects expands a project mix into a sampling slice.
func weightedProjects(mix map[string]float64) []string {
	const resolution = 1000
	keys := make([]string, 0, len(mix))
	for k := range mix {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic expansion
	var total float64
	for _, k := range keys {
		total += mix[k]
	}
	var out []string
	for _, k := range keys {
		n := int(mix[k] / total * resolution)
		for range n {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}
