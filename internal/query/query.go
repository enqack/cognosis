// Package query is hybrid retrieval: per-leg ranking happens in Postgres
// (internal/store's Rank* methods), reciprocal-rank fusion happens here. The
// fusion merge is generic over the ranked element type -- the same code path
// fuses one provider's results or a multi-provider fan-out, which is exactly
// what a live embedding migration's fallback read reuses.
package query

import (
	"context"
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
	// ftsFallbackBelow is the keyword leg's OR-fallback threshold: when the AND
	// conjunction returns fewer than this many candidates, the leg is re-run
	// with OR semantics.
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
	// archivedLinkPenalty severely discounts a fused chunk whose parent note
	// links to a soft-deleted note -- a .9-similarity stale reflection about an
	// archived concept is depressed out of the top-K rather than injected into
	// context as current truth. The append-only text stays intact.
	archivedLinkPenalty = 0.15
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
	// GraphWeight scales the graph leg; 0 keeps the package default.
	GraphWeight float64
	// FTSFallbackBelow overrides the keyword leg's OR-fallback threshold. A
	// negative value disables the fallback entirely, which zero cannot express
	// -- zero means "unset, use the default", and the sweeps need an arm that is
	// the pre-fallback engine.
	FTSFallbackBelow int
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

func (t Tuning) graphWeight() float64 {
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

		// Fall back to OR when the conjunction came back near-empty. The retry
		// is sequential rather than speculative: firing both connectives on
		// every query would double the keyword leg's database work to discard
		// one result almost always, and this path is rare by construction.
		// FTSPrimaryOr already ran the disjunction; retrying it would measure
		// the same query twice and report the second run as a fallback.
		fellBack := false
		if below := e.Tuning.ftsFallbackBelow(); !e.Tuning.FTSPrimaryOr && below > 0 && len(kw) < below {
			alt, err := e.Store.RankFTSMode(gctx, text, store.TSQueryOr, opts.filter(), pool)
			if err != nil {
				return err
			}
			// Only take the disjunction if it actually found more. Equal or
			// worse means the terms are absent from the corpus rather than
			// merely scattered across chunks, and swapping in an identical or
			// emptier leg would report a fallback that bought nothing.
			if len(alt) > len(kw) {
				kw, fellBack = alt, true
			}
		}

		primary[ftsIdx] = Leg[store.RankedChunk]{Items: kw, Weight: 1}
		statsMu.Lock()
		stats.FTS = len(kw)
		stats.FTSFallback = fellBack
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
