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
func clusterVocab(cluster int) []string {
	return topicWords[cluster%len(topicWords)]
}

// Chunk-length bounds. Uniform-length chunks make document-length
// normalization a no-op, which would make BM25 and ts_rank_cd agree for a
// reason that is a property of the fixture rather than of either ranker.
const (
	chunkMinWords = 40
	chunkMaxWords = 160
)

// topicRate is the share of a chunk's words drawn from its cluster vocabulary;
// the rest come from the shared background pool. Real prose is mostly
// connective tissue, and the imbalance is what produces an IDF spread.
const topicRate = 0.30

// skewedIndex biases toward low indices (best of two draws), so a few terms in
// each pool are frequent and the rest are rare. Flat sampling gives every term
// the same document frequency, and with uniform IDF there is nothing for a
// ranking function to weigh.
func skewedIndex(rng *rand.Rand, n int) int {
	a, b := rng.Intn(n), rng.Intn(n)
	return min(a, b)
}

// chunkProse renders one chunk of prose-like text: variable length, topic
// terms mixed into background at topicRate, both drawn with skew so term
// frequency varies within and across chunks.
func chunkProse(rng *rand.Rand, vocab []string, note, ordinal int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "note %05d section %d. ", note, ordinal)
	words := chunkMinWords + rng.Intn(chunkMaxWords-chunkMinWords+1)
	for w := range words {
		if w > 0 {
			b.WriteByte(' ')
		}
		if rng.Float64() < topicRate {
			b.WriteString(vocab[skewedIndex(rng, len(vocab))])
		} else {
			b.WriteString(backgroundWords[skewedIndex(rng, len(backgroundWords))])
		}
	}
	b.WriteByte('.')
	return b.String()
}

// queryTerms is how many terms an evaluation query carries.
// websearch_to_tsquery joins them with AND, so this is a hard conjunction:
// every extra term multiplies the chance of an empty candidate set.
const queryTerms = 3

// queryHeadTerms bounds queries to each vocabulary's frequent head. Drawing
// from the rare tail reproduces the original defect — a conjunction no chunk
// satisfies — and an FTS leg returning nothing cannot rank anything, which is
// the comparison BM25 work needs to make.
const queryHeadTerms = 4

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

	// Project and status are assigned up front, stratified, from rngs of their
	// own — not drawn inline from the shared rng.
	//
	// Both properties have already been silently destroyed once each. First by
	// colliding moduli (i%97 for project, i%25 for status) which made every
	// note in the smallest project also archived. Then by an unrelated change
	// to chunk-text generation: it consumed a different number of draws, which
	// shifted every subsequent draw and emptied the selective scope again with
	// the same seed and the same spec.
	//
	// Stratification makes the shares exact rather than expected, and separate
	// rngs mean nothing about *text* can move a note between projects. The
	// structural shape of the corpus is then a function of the spec alone.
	projectOf := assignProjects(spec, projects)
	statusOf := assignStatuses(spec, projectOf)

	// Pass 1: notes and chunks.
	allVecs := make([][]float32, 0, spec.Notes*spec.ChunksPerNote)
	for i := range notes {
		id := uuid.New()
		path := fmt.Sprintf("notes/n%05d.md", i)
		project := projectOf[i]
		status := statusOf[i]
		notes[i] = noteRec{id, path, project, status}
		if status == "archived" {
			archived = append(archived, id)
		}

		cluster := rng.Intn(spec.Clusters)
		vocab := clusterVocab(cluster)

		chunks := make([]store.Chunk, spec.ChunksPerNote)
		vecs := map[uuid.UUID][]float32{}
		texts := make([]string, spec.ChunksPerNote)
		for o := range chunks {
			text := chunkProse(rng, vocab, i, o)
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
		vocab := clusterVocab(cluster)
		var b strings.Builder
		for w := range queryTerms {
			b.WriteString(vocab[(q+w)%queryHeadTerms])
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
// Salts derive independent rng streams from one spec seed, so a corpus stays
// reproducible from Seed alone while project, status and text generation
// cannot perturb each other.
const (
	projectSalt = 0x50524f4a
	statusSalt  = 0x53544154
)

// assignProjects gives each note a project, in exact proportion to ProjectMix
// rather than in expectation. Walking the sorted weight pool at a fixed stride
// guarantees the smallest project is represented at all — a 1% project drawn
// independently per note can come up empty, and an empty selective scope
// measures nothing while looking like a passing run.
func assignProjects(spec CorpusSpec, pool []string) []string {
	out := make([]string, spec.Notes)
	for i := range out {
		out[i] = pool[i*len(pool)/spec.Notes]
	}
	rng := rand.New(rand.NewSource(spec.Seed ^ projectSalt)) //nolint:gosec // reproducibility, not secrecy
	rng.Shuffle(len(out), func(a, b int) { out[a], out[b] = out[b], out[a] })
	return out
}

// assignStatuses stratifies status *within* each project rather than across
// the corpus. That is the load-bearing detail: a global draw can put every
// note of the smallest project into falsified or archived, which empties the
// selective scope without emptying the corpus. Flooring the per-group counts
// means a project too small to afford a falsified note simply does not get
// one, so a live note always survives.
func assignStatuses(spec CorpusSpec, projectOf []string) []string {
	byProject := map[string][]int{}
	for i, p := range projectOf {
		byProject[p] = append(byProject[p], i)
	}
	names := make([]string, 0, len(byProject))
	for p := range byProject {
		names = append(names, p)
	}
	sort.Strings(names) // deterministic iteration

	out := make([]string, len(projectOf))
	rng := rand.New(rand.NewSource(spec.Seed ^ statusSalt)) //nolint:gosec // reproducibility, not secrecy
	for _, p := range names {
		idx := byProject[p]
		rng.Shuffle(len(idx), func(a, b int) { idx[a], idx[b] = idx[b], idx[a] })
		nFalse := int(float64(len(idx)) * spec.FalsifiedFrac)
		nArch := int(float64(len(idx)) * spec.ArchivedFrac)
		for j, n := range idx {
			switch {
			case j < nFalse:
				out[n] = "falsified"
			case j < nFalse+nArch:
				out[n] = "archived"
			default:
				out[n] = "active"
			}
		}
	}
	return out
}

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
