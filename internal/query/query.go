// Package query is hybrid retrieval: per-leg ranking happens in Postgres
// (internal/store's Rank* methods), reciprocal-rank fusion happens here. The
// fusion merge is generic over the ranked element type -- the same code path
// fuses one provider's results or a multi-provider fan-out, which is exactly
// what a live embedding migration's fallback read reuses.
package query

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/store"
)

const (
	// rrfK is the standard reciprocal-rank-fusion damping constant.
	rrfK = 60
	// candidatePool caps each leg's contribution before fusion.
	candidatePool = 50
	// DefaultTopK is the result count when the caller doesn't ask.
	DefaultTopK = 8
	// graphWeight scales the graph leg: a booster, not a primary signal.
	graphWeight = 0.5
	// ftsFallbackBelow is the keyword leg's AND-starvation threshold: when the AND
	// conjunction returns fewer than this many candidates, the leg escalates
	// through the fallback chain -- note-level membership first, then a bare OR as
	// the recall floor (see the chain in RunWithStats).
	//
	// websearch_to_tsquery ANDs its terms and chunking is per-heading, so a
	// query whose terms are spread across a note's sections matches none of its
	// chunks. The note is then absent, not merely demoted -- the keyword leg
	// contributes membership rather than ordering, so a candidate it never
	// produces cannot be recovered downstream.
	//
	// 2 rather than 1, and this is the whole of the choice: firing only on an
	// empty result is measurably insufficient. The real-vault query that
	// motivated this returned exactly one candidate, belonging to the wrong
	// note, and a fire-on-empty rule is byte-identical to shipped behaviour
	// there. On the 8000-chunk evaluation corpus, over the queries in exactly
	// that regime, fallback@1 left target-note recall at the shipped 0.067
	// while fallback@2 reached 0.400.
	//
	// The cost of firing too eagerly is real -- OR admits chunks matching one
	// incidental term -- which is why this is a small threshold and not a switch
	// to OR. At 2 it fired on zero of 30 healthy queries on that corpus, while
	// unconditional OR moved every one of them.
	//
	// Absolute recall is corpus-dependent (the same comparison runs 0.500 to
	// 0.917 at 125 chunks); the current numbers live in
	// internal/query/retrievaleval/testdata/tsquery_or_fallback_sweep.txt,
	// which states its corpus size in the header.
	ftsFallbackBelow = 2
	// FTSFallbackNoteLevel and FTSFallbackOr name which arm of the keyword leg's
	// AND-starvation chain produced the final candidate set. "" means the AND
	// conjunction stood on its own (or FTSPrimaryOr ran the disjunction as the
	// primary, which is not a fallback). Recorded in LegStats.FTSFallbackKind and
	// logged, so a note-level recovery and an OR floor-catch are distinguishable
	// in telemetry rather than both reading as a bare "fell back".
	FTSFallbackNoteLevel = "note-level"
	FTSFallbackOr        = "or"
	// archivedLinkPenalty severely discounts a fused chunk whose parent note
	// links to a soft-deleted note -- a .9-similarity stale reflection about an
	// archived concept is depressed out of the top-K rather than injected into
	// context as current truth. The append-only text stays intact.
	archivedLinkPenalty = 0.15
	// diversityDecay is the per-note fan-effect penalty: in fused score order, a
	// note's n-th chunk is scaled by diversityDecay^n, so its best chunk competes
	// at full strength while its redundant chunks yield top-K slots to other
	// notes. Fusion is chunk-level with no per-note constraint, so without this one
	// long note's chunks can crowd an answer a shorter relevant note never places
	// in -- measured on the real vault, the off arm returned only 5.3 distinct
	// notes of 8, one note owning 7 slots. 0.5 was the swept knee: it caps a note
	// at ~2 top-8 slots (lifting distinct sources 5.3->7.9) while still letting a
	// note that genuinely out-scores everything keep a second slot, and it left
	// cluster-relevance flat on the labelled corpus (TOPK-REL 1.000->0.996). A
	// note's n-th chunk is scaled by 0.5^n; see TestDiversityRerankSweep.
	diversityDecay = 0.5
)

// Result is one fused retrieval hit.
type Result struct {
	Path        string
	Category    string
	HeadingPath string
	Content     string
	Summary     string
	Score       float64
}

// Options are the retrieval filters.
type Options struct {
	Project          string
	TopK             int
	IncludeFalsified bool
	// IncludeArchived surfaces soft-deleted notes (faded/archived). Default
	// false: archived concepts stay out of ordinary retrieval.
	IncludeArchived bool
	// AsOf answers "what did the KB believe at time T": notes created after T
	// vanish, and a note falsified after T counts as still believed. Content
	// is always current -- recovering content-at-T is vault history's job.
	AsOf *time.Time
	// CategoryBias is a persona's retrieval lens (persona_filter): fused
	// scores are multiplied by the weight for their note's category (absent
	// categories keep weight 1). A bias, never a hard filter -- every leg
	// still runs over the full corpus.
	CategoryBias map[string]float64
}

func (o Options) filter() store.Filter {
	return store.Filter{
		Project:          o.Project,
		IncludeFalsified: o.IncludeFalsified,
		IncludeArchived:  o.IncludeArchived,
		AsOf:             o.AsOf,
	}
}

// ProviderLeg pairs an embedding provider with its table for the vector leg.
type ProviderLeg struct {
	Provider embed.Provider
	Table    string
}

// Tuning overrides the fusion constants for the retrieval evaluation harness
// (internal/query/retrievaleval). A zero field keeps the package default, so
// the zero Tuning is exactly current behavior.
//
// This is deliberately not reachable from config, CLI flags, or MCP: cognosis
// has no retrieval-tuning surface, and this does not create one. It is an
// unexported-in-spirit seam that exists so a sweep can vary one constant at a
// time without the harness reimplementing Run -- which would measure something
// that is not the retrieval engine.
//
// archivedLinkPenalty is deliberately absent. It is not a tuning knob but an
// epistemics guarantee with its own tests; making it sweepable invites someone
// to sweep it.
type Tuning struct {
	RRFK          int
	CandidatePool int
	// TopK is the harness default. opts.TopK still wins when set -- Options is
	// the caller surface, Tuning is the harness surface, and that precedence
	// must not be ambiguous.
	TopK int
	// GraphWeight scales the graph leg; 0 keeps the package default. A
	// negative value means weight zero, which zero cannot express -- and a
	// zero-weighted leg is not DisableGraph: its items still enter the fused
	// set at score 0 (see DisableGraph below).
	GraphWeight float64
	// FTSFallbackBelow overrides the keyword leg's OR-fallback threshold. A
	// negative value disables the fallback entirely, which zero cannot express
	// -- zero means "unset, use the default", and the sweeps need an arm that is
	// the pre-fallback engine.
	FTSFallbackBelow int
	// DisableNoteLevel drops the note-level-membership arm from the starvation
	// fallback chain, leaving the pre-note-level engine: AND, then a bare OR when
	// AND starves. Exists so a sweep can measure what note-level membership is
	// worth against the OR-only fallback it replaced; the request path leaves it
	// false, so the zero Tuning keeps note-level enabled -- current production.
	DisableNoteLevel bool
	// FTSPrimaryOr runs the keyword leg as a single OR query, no AND pass and
	// no fallback -- the candidate design for a traffic profile where the AND
	// conjunction starves on nearly every real query and the two-phase
	// engine's first query is overhead. Exists so the harness can price that
	// design against the shipped one; nothing in the request path sets it.
	FTSPrimaryOr bool
	// DisableGraph skips the graph leg entirely, which is NOT the same as
	// GraphWeight=0 and is the distinction that matters for the "does the
	// graph leg mask vector-leg truncation" experiment. FuseRRF accumulates
	// `weight/(k+rank)`, so a zero-weighted leg still *inserts* its items into
	// the fused set at score 0 -- they contribute nothing yet still occupy
	// top-K slots. Only skipping the leg removes them.
	DisableGraph bool
	// DiversityDecay overrides the per-note fan-effect penalty. Zero keeps the
	// package default; a negative value disables it (factor 1.0, no penalty) --
	// the "off" arm the sweep compares against, which zero cannot express since
	// zero means "unset". A value in (0,1] is the decay applied per repeated note.
	DiversityDecay float64
}

func (t Tuning) rrfK() int {
	if t.RRFK > 0 {
		return t.RRFK
	}
	return rrfK
}

func (t Tuning) candidatePool() int {
	if t.CandidatePool > 0 {
		return t.CandidatePool
	}
	return candidatePool
}

// graphWeight returns the graph leg's fusion weight; a negative override
// means weight zero, which is how a sweep asks for a zero-weighted (but still
// inserted) graph leg.
func (t Tuning) graphWeight() float64 {
	if t.GraphWeight < 0 {
		return 0
	}
	if t.GraphWeight > 0 {
		return t.GraphWeight
	}
	return graphWeight
}

// ftsFallbackBelow returns the threshold; a negative override disables the
// fallback, which is how a sweep asks for the pre-fallback engine.
func (t Tuning) ftsFallbackBelow() int {
	if t.FTSFallbackBelow != 0 {
		return t.FTSFallbackBelow
	}
	return ftsFallbackBelow
}

// diversityDecay returns the per-note fan-effect penalty; a negative override
// disables it (factor 1.0), which is how a sweep asks for the un-diversified
// ordering, and which zero -- meaning "unset" -- cannot express.
func (t Tuning) diversityDecay() float64 {
	if t.DiversityDecay < 0 {
		return 1.0
	}
	if t.DiversityDecay > 0 {
		return t.DiversityDecay
	}
	return diversityDecay
}

func (t Tuning) topK() int {
	if t.TopK > 0 {
		return t.TopK
	}
	return DefaultTopK
}

// Engine runs hybrid retrieval over one store and N provisioned providers.
type Engine struct {
	Store *store.Store
	// Providers is the static leg list. When Factory is set instead, legs are
	// built per run from the provider registry -- so a migration provisioning
	// a second table mid-flight changes retrieval without a restart, and the
	// fan-out doubles as the migration's fallback read.
	Providers []ProviderLeg
	Factory   func(name, model string) (embed.Provider, error)
	// Lazy, when set, receives the hit chunk ids after each run -- the
	// migration's touch-migration hook. Fire-and-forget on the callee's side.
	Lazy func(ids []uuid.UUID)
	// Tuning is the retrieval-evaluation seam; the zero value is production
	// behavior. Nothing in the request path sets it.
	Tuning Tuning
}

// legs resolves the vector legs for one run.
func (e *Engine) legs(ctx context.Context) ([]ProviderLeg, error) {
	if e.Factory == nil {
		return e.Providers, nil
	}
	regs, err := e.Store.Providers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderLeg, 0, len(regs))
	for _, r := range regs {
		p, err := e.Factory(r.Name, r.Model)
		if err != nil {
			return nil, err
		}
		out = append(out, ProviderLeg{Provider: p, Table: r.Table})
	}
	return out, nil
}

// LegStats reports how many candidates each leg contributed to one query,
// before fusion and before the top-K cut.
//
// It exists because the fused result count cannot answer the question it looks
// like it answers. A query returning results says nothing about whether the
// keyword leg found anything: measured on the evaluation corpus, the keyword
// leg returned 0-2 candidates of a requested 50 while fused output looked
// healthy throughout. Deciding anything about the keyword leg -- AND versus OR
// tsquery semantics, or whether a different ranker is worth an extension --
// needs per-leg counts from real traffic, and nothing was recording them.
//
// Counts only, never query text. The audit log deliberately records text_len
// rather than the text, and this keeps to that line.
type LegStats struct {
	Vector int // summed across provider legs (one per provisioned provider)
	FTS    int
	Graph  int
	Fused  int // distinct candidates after fusion, before the top-K cut

	// FTSFallback records that the keyword leg re-ran with OR semantics because
	// the AND conjunction came back below ftsFallbackBelow.
	//
	// Logged because a silent fallback has the same defect as the silent empty
	// leg this struct was created to expose: FTS=35 looks like a healthy
	// keyword leg whether it came from a conjunction that matched or a
	// disjunction papering over one that did not. The two have very different
	// precision, and telling them apart from counts alone is impossible.
	FTSFallback bool

	// FTSFallbackKind names which arm of the starvation chain produced the final
	// set: FTSFallbackNoteLevel, FTSFallbackOr, or "" when the AND conjunction
	// stood on its own. FTSFallback is the bool projection (kind != ""); the kind
	// is what separates a precise note-level recovery from an OR floor-catch,
	// which have different precision and want to be tracked apart on real traffic.
	FTSFallbackKind string

	// FTSPrimary is the candidate count of the keyword leg's first query,
	// before any fallback: the AND conjunction's own result, or the
	// disjunction's under Tuning.FTSPrimaryOr. FTS records what the leg
	// finally contributed, so when the fallback fires the pair shows how
	// starved the conjunction was -- the severity that FTSFallback alone
	// cannot express, and that the 2026-07-19 traffic analysis had to infer.
	// Equal to FTS whenever no fallback replaced the result.
	FTSPrimary int

	// FusedSources and Sources count *distinct notes*, before and after the
	// top-K cut. Fusion is chunk-level with no per-note constraint, so one long
	// note contributing many similar chunks can occupy most of the answer while
	// a shorter note about the same event never places -- and a chunk-level
	// count cannot see that, because the crowding note's chunks are all
	// genuinely relevant.
	//
	// Two numbers rather than one because they answer different questions.
	// Sources says how concentrated the *answer* was. FusedSources says whether
	// the pool held diversity that the cut then discarded: Sources far below
	// FusedSources implicates the cut, the two being equal and both low
	// implicates retrieval upstream of it. Only the first is something a
	// per-note cap could fix.
	//
	// Concentration is not automatically a defect -- a query whose answer really
	// does live in one note *should* return one note. Distinguishing that from
	// displacement needs ground truth spanning several notes, which these
	// counts cannot supply; they exist to show whether the situation arises
	// often enough on real traffic to be worth building that fixture for.
	FusedSources int
	Sources      int
}

// applyDiversityPenalty demotes a note's redundant chunks so one over-associated
// source cannot crowd the fused top-K -- the fan effect. Walking in score order,
// a note's n-th chunk (n counted from 0) is scaled by decay^n, so its best chunk
// competes at full strength while each extra yields ground to other notes; the
// list is re-sorted stably afterward. The shipped default is 0.5 and active on
// every request; decay >= 1 is the no-op early return, reachable only via
// Tuning.DiversityDecay < 0 (the sweep's "off" arm).
func applyDiversityPenalty[T any](fused []Scored[T], noteID func(T) uuid.UUID, decay float64) {
	if decay >= 1.0 || len(fused) == 0 {
		return
	}
	seen := make(map[uuid.UUID]int, len(fused))
	for i := range fused {
		id := noteID(fused[i].Item)
		if c := seen[id]; c > 0 {
			fused[i].Score *= math.Pow(decay, float64(c))
		}
		seen[id]++
	}
	sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })
}

// countSources counts distinct notes among fused candidates.
func countSources[T any](fused []Scored[T], noteID func(T) uuid.UUID) int {
	seen := make(map[uuid.UUID]struct{}, len(fused))
	for _, f := range fused {
		seen[noteID(f.Item)] = struct{}{}
	}
	return len(seen)
}

// Run embeds the query per provider, ranks all legs, and fuses.
func (e *Engine) Run(ctx context.Context, text string, opts Options) ([]Result, error) {
	out, _, err := e.RunWithStats(ctx, text, opts)
	return out, err
}

// RunWithStats is Run, additionally reporting per-leg candidate counts. Run
// delegates to it, so there is one implementation and the counts describe the
// query that actually ran.
func (e *Engine) RunWithStats(ctx context.Context, text string, opts Options) ([]Result, LegStats, error) {
	const op = "query.Run"
	var stats LegStats
	if text == "" {
		return nil, stats, cogerr.Ef(op, cogerr.Validation, "query text is required")
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = e.Tuning.topK()
	}
	pool := e.Tuning.candidatePool()

	var legs []Leg[store.RankedChunk]
	// The vector and keyword legs run concurrently, so their counters need a
	// mutex. The graph and fused counts are written after g.Wait() and do not.
	var statsMu sync.Mutex

	providerLegs, err := e.legs(ctx)
	if err != nil {
		return nil, stats, err
	}

	// The primary legs are independent -- one vector leg per provisioned provider
	// (embed + rank) plus the keyword leg -- so fan them out concurrently instead
	// of paying the sum of their latencies. Each writes its own fixed slot, so
	// the fused leg order (and the ranking it produces) is identical regardless
	// of completion order. The graph leg is not here: it is seeded by these
	// candidates and runs after.
	primary := make([]Leg[store.RankedChunk], len(providerLegs)+1)
	g, gctx := errgroup.WithContext(ctx)
	for i, pl := range providerLegs {
		g.Go(func() error {
			vec, err := pl.Provider.EmbedQuery(gctx, text)
			if err != nil {
				return cogerr.E(op, cogerr.Unavailable, err)
			}
			items, err := e.Store.RankVector(gctx, pl.Table, vec, opts.filter(), pool)
			if err != nil {
				return err
			}
			primary[i] = Leg[store.RankedChunk]{Items: items, Weight: 1}
			statsMu.Lock()
			stats.Vector += len(items)
			statsMu.Unlock()
			return nil
		})
	}
	ftsIdx := len(providerLegs)
	g.Go(func() error {
		mode := store.TSQueryWebsearch
		if e.Tuning.FTSPrimaryOr {
			mode = store.TSQueryOr
		}
		kw, err := e.Store.RankFTSMode(gctx, text, mode, opts.filter(), pool)
		if err != nil {
			return err
		}

		// AND-starvation fallback chain. When the conjunction comes back below
		// the threshold, escalate in increasing blast radius:
		//
		//   1. note-level membership -- a note whose terms are scattered across
		//      its headings AND-matches at the note level (notes.fts) though no
		//      single chunk does. Recovers the scattered target at ~10x the
		//      candidate precision of a bare OR (see the note-level-membership
		//      sweep), so it is tried first.
		//   2. OR disjunction -- the recall floor, reached only when note-level
		//      still starves (the terms are genuinely absent from any one note as
		//      a set, not merely scattered within one). It can only ever add to
		//      recall here, never subtract: it runs only when nothing better
		//      cleared the threshold.
		//
		// The retries are sequential rather than speculative and rare by
		// construction: on a healthy query AND saturates the pool and neither
		// fires. FTSPrimaryOr already ran the disjunction, so it skips the chain
		// -- retrying would measure the same query twice.
		fallbackKind := ""
		primaryCount := len(kw)
		if below := e.Tuning.ftsFallbackBelow(); !e.Tuning.FTSPrimaryOr && below > 0 && len(kw) < below {
			if !e.Tuning.DisableNoteLevel {
				nl, err := e.Store.RankFTSNoteLevel(gctx, text, opts.filter(), pool)
				if err != nil {
					return err
				}
				// Only take it if it actually found more; equal or worse means it
				// bought nothing and reporting a fallback would mislead.
				if len(nl) > len(kw) {
					kw, fallbackKind = nl, FTSFallbackNoteLevel
				}
			}
			if len(kw) < below {
				alt, err := e.Store.RankFTSMode(gctx, text, store.TSQueryOr, opts.filter(), pool)
				if err != nil {
					return err
				}
				if len(alt) > len(kw) {
					kw, fallbackKind = alt, FTSFallbackOr
				}
			}
		}

		primary[ftsIdx] = Leg[store.RankedChunk]{Items: kw, Weight: 1}
		statsMu.Lock()
		stats.FTS = len(kw)
		stats.FTSPrimary = primaryCount
		stats.FTSFallback = fallbackKind != ""
		stats.FTSFallbackKind = fallbackKind
		statsMu.Unlock()
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, stats, err
	}
	legs = append(legs, primary...)

	// Graph leg: seeded by the notes behind the candidates found so far.
	seedSet := map[uuid.UUID]bool{}
	var seeds []uuid.UUID
	for _, l := range legs {
		for _, c := range l.Items {
			if !seedSet[c.NoteID] {
				seedSet[c.NoteID] = true
				seeds = append(seeds, c.NoteID)
			}
		}
	}
	if !e.Tuning.DisableGraph {
		graph, err := e.Store.RankGraph(ctx, seeds, opts.filter(), pool)
		if err != nil {
			return nil, stats, err
		}
		stats.Graph = len(graph)
		legs = append(legs, Leg[store.RankedChunk]{Items: graph, Weight: e.Tuning.graphWeight()})
	}

	fused := FuseRRF(e.Tuning.rrfK(), func(c store.RankedChunk) uuid.UUID { return c.ChunkID }, legs)
	stats.Fused = len(fused)

	// Archived-link penalty: a chunk whose parent note still references a
	// soft-deleted note is depressed so a dense stale description of a shelved
	// concept cannot bypass the soft-delete filter via reflections/entries.
	if len(fused) > 0 {
		seen := map[uuid.UUID]bool{}
		noteIDs := make([]uuid.UUID, 0, len(fused))
		for _, f := range fused {
			if id := f.Item.NoteID; !seen[id] {
				seen[id] = true
				noteIDs = append(noteIDs, id)
			}
		}
		penalized, err := e.Store.ArchivedLinkers(ctx, noteIDs)
		if err != nil {
			return nil, stats, err
		}
		if len(penalized) > 0 {
			for i := range fused {
				if penalized[fused[i].Item.NoteID] {
					fused[i].Score *= archivedLinkPenalty
				}
			}
			sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })
		}
	}

	if len(opts.CategoryBias) > 0 {
		for i := range fused {
			if w, ok := opts.CategoryBias[fused[i].Item.Category]; ok {
				fused[i].Score *= w
			}
		}
		sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })
	}
	// Fan-effect diversity: demote a note's redundant chunks so one over-associated
	// source cannot crowd the top-K. Last of the score rewrites, so it acts on the
	// ordering the caller receives -- after the archived-link penalty and category
	// bias, and just before the cut the Sources counts describe.
	applyDiversityPenalty(fused, func(c store.RankedChunk) uuid.UUID { return c.NoteID }, e.Tuning.diversityDecay())
	// Before the cut, and after: the pair is what separates "the cut
	// concentrated the answer" from "retrieval never offered anything else".
	// Taken after the archived-link penalty and the category bias, so both
	// describe the ordering the caller actually received.
	noteOf := func(c store.RankedChunk) uuid.UUID { return c.NoteID }
	stats.FusedSources = countSources(fused, noteOf)
	if len(fused) > topK {
		fused = fused[:topK]
	}
	stats.Sources = countSources(fused, noteOf)
	out := make([]Result, len(fused))
	hitIDs := make([]uuid.UUID, len(fused))
	for i, f := range fused {
		out[i] = Result{
			Path:        f.Item.NotePath,
			Category:    f.Item.Category,
			HeadingPath: f.Item.HeadingPath,
			Content:     f.Item.Content,
			Summary:     f.Item.Summary,
			Score:       f.Score,
		}
		hitIDs[i] = f.Item.ChunkID
	}
	// Touch-migration: hot chunks migrate ahead of the back-fill's batch
	// order. Non-blocking -- the response is already assembled.
	if e.Lazy != nil && len(hitIDs) > 0 {
		e.Lazy(hitIDs)
	}
	return out, stats, nil
}
