// Measurement surface for internal/query/retrievaleval. Nothing in the request
// path calls anything in this file: the daemon never probes, never sets a
// session GUC per query, and never runs the exact-KNN variant. It lives in
// package store only because s.pool is unexported and the harness must execute
// the *production* leg SQL -- a harness that writes its own vector query
// measures something that is not the vector leg.
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
)

// SessionSettings are SET LOCAL overrides applied to a probe's transaction,
// e.g. {"hnsw.ef_search": "200", "hnsw.iterative_scan": "relaxed_order"}.
type SessionSettings map[string]string

// probeGUCs is the allowlist for SessionSettings keys. SET LOCAL takes an
// identifier, not a bind parameter, so the name is interpolated -- the
// allowlist is what makes that safe.
var probeGUCs = map[string]bool{
	"hnsw.ef_search":           true,
	"hnsw.iterative_scan":      true,
	"hnsw.max_scan_tuples":     true,
	"hnsw.scan_mem_multiplier": true,
	"ivfflat.probes":           true,
	"ivfflat.iterative_scan":   true,
	"enable_seqscan":           true,
	"enable_indexscan":         true,
}

// Probe is one instrumented leg execution.
type Probe struct {
	Rows []RankedChunk
	// Requested is the limit asked for; len(Rows) is what came back. A gap
	// means the scan could not fill the pool.
	Requested int
	Elapsed   time.Duration
	// Plan is the EXPLAIN output, when requested. Callers must confirm the
	// expected access path appears before trusting any number: a corpus small
	// enough that the planner prefers a seqscan silently voids an ANN
	// measurement by reporting a full result set and perfect recall.
	Plan string
}

// Truncated reports whether the scan returned fewer rows than asked for.
func (p Probe) Truncated() bool { return len(p.Rows) < p.Requested }

// probeTx runs fn in a transaction with set applied via SET LOCAL, and always
// rolls back -- a probe must never leave state behind, and SET LOCAL dies with
// the transaction so no pooled connection carries a tuning override home.
func (s *Store) probeTx(ctx context.Context, op string, set SessionSettings,
	fn func(context.Context, pgxTx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	for k, v := range set {
		if !probeGUCs[k] {
			return cogerr.Ef(op, cogerr.Validation, "setting %q is not in the probe allowlist", k)
		}
		if strings.ContainsAny(v, `'\`) {
			return cogerr.Ef(op, cogerr.Validation, "setting %q has an unquotable value %q", k, v)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("set local %s = '%s'", k, v)); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}
	return fn(ctx, tx)
}

// runProbe times one leg execution and optionally records its plan.
func (s *Store) runProbe(ctx context.Context, op, sql string, args []any, limit int,
	set SessionSettings, explain bool) (Probe, error) {
	p := Probe{Requested: limit}
	err := s.probeTx(ctx, op, set, func(ctx context.Context, tx pgxTx) error {
		if explain {
			plan, err := explainOf(ctx, tx, sql, args)
			if err != nil {
				return cogerr.E(op, cogerr.Internal, err)
			}
			p.Plan = plan
		}
		start := time.Now()
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
		out, err := scanRanked(rows)
		p.Elapsed = time.Since(start)
		if err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
		p.Rows = out
		return nil
	})
	if err != nil {
		return Probe{}, err
	}
	return p, nil
}

// ProbeVector runs the production vector leg under the given session settings.
func (s *Store) ProbeVector(ctx context.Context, table string, vec []float32,
	f Filter, limit int, set SessionSettings, explain bool) (Probe, error) {
	const op = "store.ProbeVector"
	if !tableNameRe.MatchString(table) {
		return Probe{}, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	return s.runProbe(ctx, op, vectorLegSQL(table, false), vectorLegArgs(vec, f, limit),
		limit, set, explain)
}

// ProbeVectorExact runs the same leg with index matching defeated: the
// brute-force nearest neighbours over a byte-identical filter scope. This is
// the ground truth recall is measured against -- the vector leg's contract is
// "the k nearest by cosine", so divergence from brute force is by definition
// the defect being hunted, not a proxy for it.
//
// No GUC is forced: the `+ 0.0` in vectorLegSQL defeats HNSW matching on its
// own and is planner-independent, whereas enable_indexscan=off would also
// disable the pkey and join index scans and measure a different plan shape.
// explain is always on so the caller can *verify* the scan was exact rather
// than assume it -- "brute force is ground truth" only holds if brute force
// actually bypassed the index.
func (s *Store) ProbeVectorExact(ctx context.Context, table string, vec []float32,
	f Filter, limit int) (Probe, error) {
	const op = "store.ProbeVectorExact"
	if !tableNameRe.MatchString(table) {
		return Probe{}, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	return s.runProbe(ctx, op, vectorLegSQL(table, true), vectorLegArgs(vec, f, limit),
		limit, nil, true)
}

// ProbeFTS runs the production keyword leg under the given session settings.
func (s *Store) ProbeFTS(ctx context.Context, text string, f Filter, limit int,
	set SessionSettings, explain bool) (Probe, error) {
	const op = "store.ProbeFTS"
	return s.runProbe(ctx, op, ftsLegSQL(), ftsLegArgs(text, f, limit), limit, set, explain)
}

// ProbeFTSMode is ProbeFTS with the tsquery connective under test. Only the
// keyword leg's candidate *set* changes between modes; filters, ranking
// expression and limit are byte-identical, so a difference in results is
// attributable to AND-versus-OR and nothing else.
func (s *Store) ProbeFTSMode(ctx context.Context, text string, mode TSQueryMode, f Filter,
	limit int, set SessionSettings, explain bool) (Probe, error) {
	const op = "store.ProbeFTSMode"
	return s.runProbe(ctx, op, ftsLegSQLMode(mode), ftsLegArgs(text, f, limit), limit, set, explain)
}

// ProbeFTSNoteLevel is the instrumented form of the note-level-membership
// keyword leg (RankFTSNoteLevel / ftsLegNoteLevelSQL) -- the production fallback,
// probed so the harness can price it against AND/OR. Args and limit match
// ProbeFTSMode exactly, so a difference in results is attributable to the
// membership rule alone.
func (s *Store) ProbeFTSNoteLevel(ctx context.Context, text string, f Filter,
	limit int, set SessionSettings, explain bool) (Probe, error) {
	const op = "store.ProbeFTSNoteLevel"
	return s.runProbe(ctx, op, ftsLegNoteLevelSQL(), ftsLegArgs(text, f, limit), limit, set, explain)
}

// ProbeGraph runs the production graph leg under the given session settings.
func (s *Store) ProbeGraph(ctx context.Context, seeds []uuid.UUID, f Filter, limit int,
	set SessionSettings, explain bool) (Probe, error) {
	const op = "store.ProbeGraph"
	if len(seeds) == 0 {
		return Probe{Requested: limit}, nil
	}
	return s.runProbe(ctx, op, graphLegSQL(), graphLegArgs(seeds, f, limit), limit, set, explain)
}

// CurrentSetting reads a session GUC from a pooled connection. Used to assert
// that Connect's scan settings actually reached the database -- the constants
// agreeing with each other is necessary but not sufficient.
func (s *Store) CurrentSetting(ctx context.Context, name string) (string, error) {
	const op = "store.CurrentSetting"
	var v string
	if err := s.pool.QueryRow(ctx, "select current_setting($1)", name).Scan(&v); err != nil {
		return "", cogerr.E(op, cogerr.Internal, err)
	}
	return v, nil
}

// explainOf returns the plan for sql, newline-joined.
func explainOf(ctx context.Context, tx pgxTx, sql string, args []any) (string, error) {
	rows, err := tx.Query(ctx, "explain "+sql, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}
