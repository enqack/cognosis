package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/enqack/cognosis/internal/cogerr"
)

// The three retrieval rankers. The database does per-ranker ranking (it's
// good at that); reciprocal-rank fusion happens in Go (internal/query), so
// each leg returns an ordered candidate list, rank = position.

// RankedChunk is one candidate from a single leg, in leg order.
type RankedChunk struct {
	ChunkID     uuid.UUID
	NoteID      uuid.UUID
	NotePath    string
	Category    string
	HeadingPath string
	Content     string
	Summary     string
}

// Filter is the shared retrieval scope applied by every leg.
type Filter struct {
	Project          string
	IncludeFalsified bool
	// IncludeArchived surfaces soft-deleted notes (status faded/archived).
	// Default false: an archived concept is shelved, so its own chunks must not
	// come back in ordinary retrieval — that is the whole point of a soft
	// delete. Audit/history callers set it true.
	IncludeArchived bool
	// AsOf reasons over frontmatter timestamps: only notes created at or
	// before T are visible, and a note falsified or archived *after* T counts
	// as believed/live at T (at-T status overrides current status). Content is
	// always current — recovering content-at-T is the vault history's job.
	AsOf *time.Time
}

// timeFilterSQL is the temporal predicate shared by the legs. Parameters:
// $if = include_falsified (bool), $ia = include_archived (bool),
// $ts = as_of (nullable timestamptz), $tx = as_of formatted as the frontmatter
// timestamp layout (text) — the falsified_at/archived_at comparisons are
// lexicographic over the fixed-width layout, which sidesteps timezone coercion
// between JSONB text and timestamptz.
func timeFilterSQL(ifP, iaP, tsP, txP string) string {
	return fmt.Sprintf(`(
	  (%[3]s::timestamptz is null
	      and (%[1]s or n.status is distinct from 'falsified')
	      and (%[2]s or n.status not in ('faded', 'archived')))
	  or (%[3]s::timestamptz is not null and n.created <= %[3]s
	      and (%[1]s or n.status is distinct from 'falsified'
	           or coalesce(n.frontmatter->>'falsified_at', '') > %[4]s)
	      and (%[2]s or n.status not in ('faded', 'archived')
	           or coalesce(n.frontmatter->>'archived_at', '') > %[4]s))
	)`, ifP, iaP, tsP, txP)
}

// asOfParams renders the two as-of parameters (nullable timestamp + text form).
func asOfParams(f Filter) (any, string) {
	if f.AsOf == nil {
		return nil, ""
	}
	return f.AsOf.UTC(), f.AsOf.Format("2006-01-02 15:04:05")
}

func scanRanked(rows pgx.Rows) ([]RankedChunk, error) {
	defer rows.Close()
	var out []RankedChunk
	for rows.Next() {
		var r RankedChunk
		if err := rows.Scan(&r.ChunkID, &r.NoteID, &r.NotePath, &r.Category, &r.HeadingPath, &r.Content, &r.Summary); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const rankedCols = `c.id, n.id, n.path, n.category, coalesce(c.heading_path, ''), c.content, n.summary`

// vectorLegSQL renders the vector leg. Callers must have validated table
// against tableNameRe first — it is interpolated, not parameterized.
//
// exact defeats index matching on the order-by expression: pgvector only
// matches the bare `<=>` operator to an HNSW index, so `+ 0.0` forces an exact
// scan regardless of GUCs or cost estimates. That is how the retrieval
// evaluation harness gets brute-force ground truth over a byte-identical
// filter scope — scoring the approximate leg against a differently-shaped
// query would measure something that is not the vector leg.
func vectorLegSQL(table string, exact bool) string {
	order := "e.embedding <=> $1"
	if exact {
		order = "(e.embedding <=> $1) + 0.0"
	}
	return fmt.Sprintf(`
		select `+rankedCols+`
		from chunks c
		join notes n on n.path = c.note_path
		join %s e on e.chunk_id = c.id
		where ($2 = '' or n.project = $2)
		  and `+timeFilterSQL("$3", "$7", "$5", "$6")+`
		order by %s
		limit $4`, table, order)
}

// vectorLegArgs are the bind parameters for vectorLegSQL, in $1..$7 order.
func vectorLegArgs(vec []float32, f Filter, limit int) []any {
	asOfTS, asOfText := asOfParams(f)
	return []any{pgvector.NewVector(vec), f.Project, f.IncludeFalsified,
		limit, asOfTS, asOfText, f.IncludeArchived}
}

// RankVector is the semantic leg: cosine distance over one provider table.
func (s *Store) RankVector(ctx context.Context, table string, vec []float32,
	f Filter, limit int) ([]RankedChunk, error) {
	const op = "store.RankVector"
	if !tableNameRe.MatchString(table) {
		return nil, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	rows, err := s.pool.Query(ctx, vectorLegSQL(table, false), vectorLegArgs(vec, f, limit)...)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out, err := scanRanked(rows)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	return out, nil
}

// ftsLegSQL renders the keyword leg.
func ftsLegSQL() string {
	return `
		select ` + rankedCols + `
		from chunks c
		join notes n on n.path = c.note_path,
		websearch_to_tsquery('english', $1) q
		where c.fts @@ q
		  and ($2 = '' or n.project = $2)
		  and ` + timeFilterSQL("$3", "$7", "$5", "$6") + `
		order by ts_rank_cd(c.fts, q) desc
		limit $4`
}

// ftsLegArgs are the bind parameters for ftsLegSQL, in $1..$7 order.
func ftsLegArgs(text string, f Filter, limit int) []any {
	asOfTS, asOfText := asOfParams(f)
	return []any{text, f.Project, f.IncludeFalsified,
		limit, asOfTS, asOfText, f.IncludeArchived}
}

// RankFTS is the keyword leg: websearch-style full-text match ranked by
// ts_rank_cd.
func (s *Store) RankFTS(ctx context.Context, text string,
	f Filter, limit int) ([]RankedChunk, error) {
	const op = "store.RankFTS"
	rows, err := s.pool.Query(ctx, ftsLegSQL(), ftsLegArgs(text, f, limit)...)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out, err := scanRanked(rows)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	return out, nil
}

// graphLegSQL renders the graph leg.
func graphLegSQL() string {
	return `
		select ` + rankedCols + `
		from links l
		join notes n on n.id = l.dst_note_id
		join chunks c on c.note_path = n.path
		where l.src_note_id = any($1)
		  and ` + timeFilterSQL("$2", "$6", "$4", "$5") + `
		group by c.id, n.id, n.path, n.category, c.heading_path, c.content, n.summary
		order by count(distinct l.src_note_id) desc
		limit $3`
}

// graphLegArgs are the bind parameters for graphLegSQL, in $1..$6 order.
func graphLegArgs(seeds []uuid.UUID, f Filter, limit int) []any {
	asOfTS, asOfText := asOfParams(f)
	return []any{seeds, f.IncludeFalsified, limit, asOfTS, asOfText, f.IncludeArchived}
}

// RankGraph is the graph leg: one hop out along links from the seed notes
// (the notes behind the other legs' candidates), chunks of linked-to notes
// ranked by how many distinct seeds reach them. A booster leg — it can
// surface a note no text or vector match would find. Project scoping is
// deliberately absent (it inherits through the seeds); the temporal and
// falsified filters apply.
func (s *Store) RankGraph(ctx context.Context, seeds []uuid.UUID,
	f Filter, limit int) ([]RankedChunk, error) {
	const op = "store.RankGraph"
	if len(seeds) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, graphLegSQL(), graphLegArgs(seeds, f, limit)...)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out, err := scanRanked(rows)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	return out, nil
}

// ArchivedLinkers returns the subset of the given note ids that hold an
// outbound link to a soft-deleted (faded/archived) note. The RRF fusion layer
// uses it to penalize chunks that describe a shelved concept: a dense old
// reflection referencing a now-archived note stays in the index (the log is
// append-only) but must not rank as current truth — the epistemological leak a
// status filter on the note itself cannot catch.
func (s *Store) ArchivedLinkers(ctx context.Context, noteIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	const op = "store.ArchivedLinkers"
	if len(noteIDs) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		select distinct l.src_note_id
		from links l
		join notes n on n.id = l.dst_note_id
		where l.src_note_id = any($1)
		  and n.status in ('faded', 'archived')`, noteIDs)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ExplainRankVector captures the planner's strategy for the vector leg — the
// test suite records it as an artifact proving the HNSW index is used.
//
// It explains the *production* leg SQL, filters and all. An earlier version
// explained a stripped query (no notes join, no WHERE, `select c.id`, a
// hardcoded limit) and so could not show the thing most worth knowing: the
// planner chooses a different access path depending on scope selectivity —
// HNSW on a broad scope, a pkey scan plus exact Sort on a narrow one. An
// artifact from the unfiltered query cannot catch a regression in the filtered
// one, which is where filtered-ANN recall actually lives.
func (s *Store) ExplainRankVector(ctx context.Context, table string, vec []float32,
	f Filter, limit int) (string, error) {
	const op = "store.ExplainRankVector"
	if !tableNameRe.MatchString(table) {
		return "", cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	rows, err := s.pool.Query(ctx, "explain "+vectorLegSQL(table, false),
		vectorLegArgs(vec, f, limit)...)
	if err != nil {
		return "", cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", cogerr.E(op, cogerr.Internal, err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	return plan.String(), rows.Err()
}
