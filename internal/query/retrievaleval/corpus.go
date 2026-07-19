package retrievaleval

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
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

	// BorrowedTerms controls keyword precision by letting each cluster's
	// vocabulary carry some of its neighbour's head words: 0 makes the keyword
	// leg perfectly precise (a conjunction can only match its own cluster) and
	// therefore makes any ranker comparison vacuous, which is not a corpus
	// anyone should measure ranking on.
	BorrowedTerms int

	Dim      int
	Clusters int
	Spread   float64
	Seed     int64

	// Queries is how many evaluation queries to generate. Recall must be
	// averaged over many queries: HNSW is not monotone per-query, so a
	// single-query measurement is noise.
	Queries int

	// StarveQueries is how many cross-chunk queries to generate: each draws one
	// distinctive term from each of StarveSections distinct chunks of a single
	// target note, so no one chunk holds all of them and the conjunction
	// matches nothing. They land in Corpus.StarveQueries, never in
	// Corpus.Queries, so every existing measurement is untouched.
	//
	// This reproduces the real-vault failure. Neither existing query set can:
	// head-drawn terms are frequent by construction, and tail-drawn terms still
	// come from a 12-term cluster vocabulary that a 40-160 word chunk very
	// nearly exhausts, so any conjunction over it is satisfiable. Starvation
	// needs terms rare *within a note*.
	StarveQueries int
	// StarveSections is how many distinct chunks a starving query draws from,
	// and therefore its term count. 4 matches the real-vault query.
	StarveSections int
	// DistinctivePerChunk is how many distinctive markers each chunk carries
	// that its siblings in the same note do not. Zero disables the whole
	// mechanism and leaves generated text byte-identical to before it existed.
	DistinctivePerChunk int
	// DistinctiveVocab bounds the marker pool, and so sets how many chunks
	// corpus-wide share a marker. Too large and OR is trivially perfect; too
	// small and the target drowns in collisions. Non-positive scales it to
	// Notes, which holds collisions per marker roughly constant as the corpus
	// grows -- a fixed pool silently changes the experiment with corpus size.
	DistinctiveVocab int
	// StarvePartialFrac is the share of starving queries given a decoy: a chunk
	// of some *other* note carrying the query's entire conjunction, so AND
	// returns exactly one candidate and that candidate is the wrong note.
	//
	// Without this the corpus starves totally -- AND returns zero for every
	// starving query, every threshold N>=1 fires on all of them, and the sweep
	// cannot separate N=1 from N=2. Partial starvation is the regime the real
	// vault was in (one candidate, belonging to a different note, target absent
	// from the fused top-6) and the only one in which the threshold is a real
	// choice rather than a formality.
	StarvePartialFrac float64
}

// DefaultSpec is sized from the Phase 0 finding that the planner chooses a
// seqscan below ~5k chunks -- measuring ANN recall on a corpus the planner
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
		BorrowedTerms:    DefaultBorrowedTerms,
		Dim:              768,
		Clusters:         40,
		Spread:           DefaultSpread,
		Seed:             7,
		Queries:          30,

		StarveQueries:       30,
		StarveSections:      4,
		DistinctivePerChunk: 2,
		StarvePartialFrac:   0.5,
	}
}

// EvalQuery is one evaluation probe with its pseudo-relevance label.
type EvalQuery struct {
	Text    string
	Cluster int
}

// StarveQuery is a query whose terms are distributed across different chunks of
// one note, so no single chunk contains all of them.
//
// Ground truth is a NOTE, not a cluster. The cluster label cannot distinguish
// the target from the ~40 other notes sharing its topic, and "did the right
// note come back" is the question the real-vault failure actually asked. The
// cluster is retained so cluster-precision stays comparable with EvalQuery.
type StarveQuery struct {
	Text     string
	NoteID   uuid.UUID
	NotePath string
	Cluster  int
	Sections []int // chunk ordinals the terms were drawn from
	// HasDecoy marks the partially-starving queries: some other note carries
	// this whole conjunction in one chunk, so AND returns exactly one candidate
	// and it is the wrong note. Queries without a decoy starve totally.
	HasDecoy bool
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

	// StarveQueries are cross-chunk conjunctions no single chunk satisfies.
	// Separate from Queries because they are a different experiment with a
	// different ground truth: Queries measures ranking on a leg that always has
	// candidates, these measure what happens when it has none.
	StarveQueries []StarveQuery

	// InScope counts live (non-falsified, non-archived) chunks per project
	// key, so tests can distinguish "the scan truncated" from "the scope only
	// holds this many rows" -- a distinction Phase 0 got wrong once already.
	InScope map[string]int
}

// clusterVocab returns the deterministic word bag for a cluster: its own topic
// words, plus a borrowed head from its neighbour interleaved at middling
// frequency.
//
// The borrowing is load-bearing and was added after a measurement failed for
// its absence. With strictly disjoint vocabularies a conjunction drawn from
// cluster c's words can only match cluster c's chunks, so the keyword leg runs
// at 100% precision -- measured: 1500 of 1500 candidates relevant. A leg that is
// never wrong cannot be improved, so an oracle re-ranking changed nothing and
// the ceiling experiment reported a confident "no headroom" that was purely an
// artifact of the fixture.
//
// Overlap does not weaken the ground truth, which is the reason disjointness
// was chosen in the first place: a chunk's label comes from the cluster that
// generated it, so a neighbour's chunk surfacing on a borrowed term is
// correctly labelled irrelevant. What overlap removes is the *identity*
// between "matches the query" and "is relevant" -- and that identity is exactly
// what has to be breakable for ranking to be measurable at all.
func clusterVocab(cluster, borrowedTerms int) []string {
	own := topicWords[cluster%len(topicWords)]
	borrowed := topicWords[(cluster+len(topicWords)-1)%len(topicWords)][:borrowedTerms]
	// Interleaved rather than appended: skewedIndex favours low indices, so
	// borrowed terms parked at the tail would be too rare to ever produce a
	// cross-cluster match, which is the whole point of borrowing them.
	// split guards BorrowedTerms=0 and 1: the zero case is a real sweep point
	// (a perfectly precise keyword leg is the control for any headroom claim),
	// so it has to build a vocabulary rather than panic on a slice bound.
	split := min(2, len(borrowed))
	out := make([]string, 0, len(own)+len(borrowed))
	out = append(out, own[:4]...)
	out = append(out, borrowed[:split]...)
	out = append(out, own[4:]...)
	out = append(out, borrowed[split:]...)
	return out
}

// DefaultBorrowedTerms is the CorpusSpec default: how many of the neighbouring
// cluster's head words each vocabulary carries. Queries draw from their own
// head (see queryHeadTerms), so this is precisely the channel by which a query
// retrieves a chunk that is textually plausible and semantically wrong -- which
// makes it the knob that sets keyword precision, and therefore how much room
// any keyword ranker has to be better than another.
const DefaultBorrowedTerms = 4

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
// distinctive markers are interleaved rather than appended, for the same reason
// clusterVocab interleaves borrowed terms: a run of markers parked at the tail
// is a different lexical shape from a term that recurs through a section, and
// ts_rank_cd's proximity component would see the difference. Passing a nil
// distinctive slice reproduces the pre-marker text byte for byte.
func chunkProse(rng *rand.Rand, vocab, distinctive []string, note, ordinal int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "note %05d section %d. ", note, ordinal)
	words := chunkMinWords + rng.Intn(chunkMaxWords-chunkMinWords+1)

	// Positions are derived from the word count, not drawn from rng, so adding
	// markers consumes no extra values from the shared stream.
	marks := map[int]string{}
	if len(distinctive) > 0 {
		slot := 0
		for _, term := range distinctive {
			for r := range distinctiveRepeat {
				pos := (words * (slot + 1)) / (len(distinctive)*distinctiveRepeat + 1)
				marks[pos+r] = term
				slot++
			}
		}
	}

	for w := range words {
		if w > 0 {
			b.WriteByte(' ')
		}
		if term, ok := marks[w]; ok {
			b.WriteString(term)
			continue
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
// from the rare tail reproduces the original defect -- a conjunction no chunk
// satisfies -- and an FTS leg returning nothing cannot rank anything, which is
// the comparison BM25 work needs to make.
const queryHeadTerms = 4

// distinctiveSalt keeps marker draws off the shared rng. Drawing them inline
// would consume a different number of values and shift every subsequent draw,
// which is exactly how project and status assignment were silently destroyed
// once before (see the comment above assignProjects).
const distinctiveSalt = 0x44495354

// distinctiveRepeat is how many times a marker is repeated inside its own
// chunk. Without repetition every chunk holding a marker has term frequency 1,
// ts_rank_cd cannot separate the target from an unrelated chunk that collided
// on the same marker, and the OR arm would be measuring a coin flip. Real notes
// mention their distinctive terms more than once in the section about them.
const distinctiveRepeat = 3

// distinctiveVocabSize resolves the marker pool size for a spec.
func distinctiveVocabSize(spec CorpusSpec) int {
	n := spec.DistinctiveVocab
	if n <= 0 {
		n = spec.Notes
	}
	return min(max(n, 1), len(distinctiveWords))
}

// distinctiveForNote returns one marker slice per chunk of a note. Markers are
// unique within the note -- that is the property that starves a conjunction
// spanning sections -- but drawn from a shared pool, so they stay rare rather
// than unique corpus-wide and the OR arm has genuine noise to rank against.
func distinctiveForNote(spec CorpusSpec, note int) [][]string {
	if spec.DistinctivePerChunk <= 0 {
		return nil
	}
	//nolint:gosec // reproducibility, not secrecy
	rng := rand.New(rand.NewSource(spec.Seed ^ distinctiveSalt ^ int64(note)))
	poolSize := distinctiveVocabSize(spec)
	used := map[string]bool{}
	out := make([][]string, spec.ChunksPerNote)
	for o := range out {
		terms := make([]string, 0, spec.DistinctivePerChunk)
		for len(terms) < spec.DistinctivePerChunk {
			w := distinctiveWords[rng.Intn(poolSize)]
			if used[w] {
				continue
			}
			used[w] = true
			terms = append(terms, w)
		}
		out[o] = terms
	}
	return out
}

// starvePlan is decided before any chunk text is generated, because a decoy has
// to be written *into* a chunk and pass 1 is the only place that happens.
// Markers are a pure function of spec and note index (distinctiveForNote), and
// status is assigned up front, so the whole plan is derivable early.
type starvePlan struct {
	targets []int // note indices carrying a starving query, in order
	// terms is the query's conjunction, one marker per section of the target.
	terms map[int][]string
	// decoyFor maps a decoy note index to the conjunction planted in its first
	// chunk. The decoy is never the target: the point is that AND surfaces one
	// candidate and it is the wrong note.
	decoyFor map[int][]string
	hasDecoy map[int]bool // target index -> was a decoy planted
}

func planStarve(spec CorpusSpec, statusOf []string) *starvePlan {
	p := &starvePlan{
		terms:    map[int][]string{},
		decoyFor: map[int][]string{},
		hasDecoy: map[int]bool{},
	}
	if spec.StarveQueries <= 0 || spec.DistinctivePerChunk <= 0 {
		return p
	}
	sections := min(spec.StarveSections, spec.ChunksPerNote)

	// Targets are taken from the front, decoys from the back, so the two never
	// collide and neither depends on a random draw that could shift.
	decoyCursor := len(statusOf) - 1
	for i := 0; i < len(statusOf) && len(p.targets) < spec.StarveQueries; i++ {
		if statusOf[i] != "active" {
			continue
		}
		marks := distinctiveForNote(spec, i)
		if len(marks) < sections {
			continue
		}
		terms := make([]string, 0, sections)
		for o := range sections {
			terms = append(terms, marks[o][0])
		}
		p.targets = append(p.targets, i)
		p.terms[i] = terms

		// Plant a decoy for the first StarvePartialFrac share of targets.
		// Deliberately a prefix rather than a random draw: the count is then
		// exact rather than expected, matching how project and status shares
		// are stratified above.
		wantDecoys := int(float64(spec.StarveQueries) * spec.StarvePartialFrac)
		if len(p.targets) > wantDecoys {
			continue
		}
		for decoyCursor > i {
			if statusOf[decoyCursor] == "active" && len(p.decoyFor[decoyCursor]) == 0 {
				p.decoyFor[decoyCursor] = terms
				p.hasDecoy[i] = true
				decoyCursor--
				break
			}
			decoyCursor--
		}
	}
	return p
}

// Build seeds the corpus and returns a wired Engine. Skips when
// COGNOSIS_TEST_DSN is unset.
