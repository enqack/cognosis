package retrievaleval

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
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

// Corpus construction: Build indexes the synthetic vault and wires scopes.
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
		cluster int
		// distinct is this note's per-chunk markers, retained so starving
		// queries can be built from chunks of a note that actually exists.
		distinct [][]string
	}
	notes := make([]noteRec, spec.Notes)
	var archived []uuid.UUID
	inScope := map[string]int{}

	// Project and status are assigned up front, stratified, from rngs of their
	// own -- not drawn inline from the shared rng.
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
	plan := planStarve(spec, statusOf)

	// Pass 1: notes and chunks.
	allVecs := make([][]float32, 0, spec.Notes*spec.ChunksPerNote)
	for i := range notes {
		id := uuid.New()
		path := fmt.Sprintf("notes/n%05d.md", i)
		project := projectOf[i]
		status := statusOf[i]
		cluster := rng.Intn(spec.Clusters)
		distinct := distinctiveForNote(spec, i)
		notes[i] = noteRec{id, path, project, status, cluster, distinct}
		if status == "archived" {
			archived = append(archived, id)
		}

		vocab := clusterVocab(cluster, spec.BorrowedTerms)

		chunks := make([]store.Chunk, spec.ChunksPerNote)
		vecs := map[uuid.UUID][]float32{}
		texts := make([]string, spec.ChunksPerNote)
		for o := range chunks {
			var marks []string
			if o < len(distinct) {
				marks = distinct[o]
			}
			// A decoy carries some other note's whole conjunction in one chunk,
			// which is what turns total starvation into partial: AND then
			// returns exactly this chunk, and it is the wrong note.
			if o == 0 {
				if decoy, ok := plan.decoyFor[i]; ok {
					marks = append(append([]string{}, marks...), decoy...)
				}
			}
			text := chunkProse(rng, vocab, marks, i, o)
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
	// path choices are neither production-like nor stable -- a freshly seeded
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
		vocab := clusterVocab(cluster, spec.BorrowedTerms)
		var b strings.Builder
		for w := range queryTerms {
			b.WriteString(vocab[(q+w)%queryHeadTerms])
			b.WriteByte(' ')
		}
		text := strings.TrimSpace(b.String())
		syn.Labels[text] = cluster
		queries[q] = EvalQuery{Text: text, Cluster: cluster}
	}

	// Starving queries: one marker from each of StarveSections distinct chunks
	// of a single target note. No chunk holds all of them, so the conjunction
	// matches nothing while every individual term is present somewhere in the
	// target -- the real-vault failure exactly.
	//
	// Targets are restricted to active notes. A falsified or archived target is
	// filtered out of every leg, so every arm would report recall 0 and the
	// table would read "the fallback does not help" when what it measured was
	// the fixture.
	starve := make([]StarveQuery, 0, len(plan.targets))
	for _, i := range plan.targets {
		n := notes[i]
		terms := plan.terms[i]
		ords := make([]int, 0, len(terms))
		for o := range len(terms) {
			ords = append(ords, o)
		}

		// The premise, checked before anything depends on it: no single chunk
		// of the *target* may carry every term. A decoy elsewhere is the point;
		// one inside the target would mean the query is not starving at all and
		// every measurement built on it is void.
		for o, chunkTerms := range n.distinct {
			have := 0
			for _, want := range terms {
				if slices.Contains(chunkTerms, want) {
					have++
				}
			}
			if have == len(terms) {
				t.Fatalf("starving query for %s is satisfiable by chunk %d of the target "+
					"itself: markers are not unique per chunk", n.path, o)
			}
		}

		text := strings.Join(terms, " ")
		syn.Labels[text] = n.cluster
		starve = append(starve, StarveQuery{
			Text: text, NoteID: n.id, NotePath: n.path,
			Cluster: n.cluster, Sections: ords, HasDecoy: plan.hasDecoy[i],
		})
	}

	return &Corpus{
		Store: s, DSN: dsn, Table: table, Provider: syn,
		Engine: &query.Engine{
			Store:     s,
			Providers: []query.ProviderLeg{{Provider: syn, Table: table}},
		},
		Queries: queries, StarveQueries: starve, Spec: spec, InScope: inScope,
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

// analyzeCorpus refreshes planner statistics on the seeded tables and merges
// index maintenance state. It opens a direct connection because these are
// maintenance commands with no business on the Store API -- nothing in the
// daemon ever needs them.
//
// VACUUM (ANALYZE) rather than plain ANALYZE: a manual ANALYZE does not merge
// the GIN fast-update pending list on chunks_fts_idx, so keyword-leg queries
// after a fresh seed pay a linear scan of pending pages until autovacuum
// happens to run -- which made BenchmarkFTSLeg bimodal (2.3ms vs 1.5ms on the
// same corpus, depending on autovacuum phase). VACUUM merges the list, so
// measurements see the steady-state index deterministically.
func analyzeCorpus(ctx context.Context, t testing.TB, dsn, table string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("vacuum analyze: connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, tbl := range []string{"notes", "chunks", "links", table} {
		if _, err := conn.Exec(ctx, "vacuum (analyze) "+pgx.Identifier{tbl}.Sanitize()); err != nil {
			t.Fatalf("vacuum analyze %s: %v", tbl, err)
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
// guarantees the smallest project is represented at all -- a 1% project drawn
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
