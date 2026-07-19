// Package query is hybrid retrieval: per-leg ranking happens in Postgres
// (internal/store's Rank* methods), reciprocal-rank fusion happens here. The
// fusion merge is generic over the ranked element type — the same code path
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
	// archivedLinkPenalty severely discounts a fused chunk whose parent note
	// links to a soft-deleted note — a .9-similarity stale reflection about an
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
	// is always current — recovering content-at-T is vault history's job.
	AsOf *time.Time
	// CategoryBias is a persona's retrieval lens (persona_filter): fused
	// scores are multiplied by the weight for their note's category (absent
	// categories keep weight 1). A bias, never a hard filter — every leg
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
// time without the harness reimplementing Run — which would measure something
// that is not the retrieval engine.
//
// archivedLinkPenalty is deliberately absent. It is not a tuning knob but an
// epistemics guarantee with its own tests; making it sweepable invites someone
// to sweep it.
type Tuning struct {
	RRFK          int
	CandidatePool int
	// TopK is the harness default. opts.TopK still wins when set — Options is
	// the caller surface, Tuning is the harness surface, and that precedence
	// must not be ambiguous.
	TopK int
	// GraphWeight scales the graph leg; 0 keeps the package default.
	GraphWeight float64
	// DisableGraph skips the graph leg entirely, which is NOT the same as
	// GraphWeight=0 and is the distinction that matters for the "does the
	// graph leg mask vector-leg truncation" experiment. FuseRRF accumulates
	// `weight/(k+rank)`, so a zero-weighted leg still *inserts* its items into
	// the fused set at score 0 — they contribute nothing yet still occupy
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
	// built per run from the provider registry — so a migration provisioning
	// a second table mid-flight changes retrieval without a restart, and the
	// fan-out doubles as the migration's fallback read.
	Providers []ProviderLeg
	Factory   func(name, model string) (embed.Provider, error)
	// Lazy, when set, receives the hit chunk ids after each run — the
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
// healthy throughout. Deciding anything about the keyword leg — AND versus OR
// tsquery semantics, or whether a different ranker is worth an extension —
// needs per-leg counts from real traffic, and nothing was recording them.
//
// Counts only, never query text. The audit log deliberately records text_len
// rather than the text, and this keeps to that line.
type LegStats struct {
	Vector int // summed across provider legs (one per provisioned provider)
	FTS    int
	Graph  int
	Fused  int // distinct candidates after fusion, before the top-K cut
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

	// The primary legs are independent — one vector leg per provisioned provider
	// (embed + rank) plus the keyword leg — so fan them out concurrently instead
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
		kw, err := e.Store.RankFTS(gctx, text, opts.filter(), pool)
		if err != nil {
			return err
		}
		primary[ftsIdx] = Leg[store.RankedChunk]{Items: kw, Weight: 1}
		statsMu.Lock()
		stats.FTS = len(kw)
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
	if len(fused) > topK {
		fused = fused[:topK]
	}
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
	// order. Non-blocking — the response is already assembled.
	if e.Lazy != nil && len(hitIDs) > 0 {
		e.Lazy(hitIDs)
	}
	return out, stats, nil
}
