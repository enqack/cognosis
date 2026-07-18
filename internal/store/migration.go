package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Migration is one embedding-provider migration's state row.
type Migration struct {
	ID             uuid.UUID
	FromName       string
	FromModel      string
	FromTable      string
	ToName         string
	ToModel        string
	ToTable        string
	Status         string // in_progress | complete | rolled_back
	Paused         bool
	ChunksTotal    int
	ChunksBackfill int
	ChunksLazy     int
	ChunksFailed   int
	StartedAt      time.Time
	FinishedAt     *time.Time
	LastError      string
}

const migrationCols = `id, from_name, from_model, from_table, to_name, to_model, to_table,
	status, paused, chunks_total, chunks_backfill, chunks_lazy, chunks_failed,
	started_at, finished_at, last_error`

func scanMigration(row pgx.Row) (Migration, error) {
	var m Migration
	err := row.Scan(&m.ID, &m.FromName, &m.FromModel, &m.FromTable, &m.ToName, &m.ToModel, &m.ToTable,
		&m.Status, &m.Paused, &m.ChunksTotal, &m.ChunksBackfill, &m.ChunksLazy, &m.ChunksFailed,
		&m.StartedAt, &m.FinishedAt, &m.LastError)
	return m, err
}

// StartMigration inserts the in_progress row; the partial unique index turns
// a concurrent second start into a Conflict.
func (s *Store) StartMigration(ctx context.Context, m Migration) (uuid.UUID, error) {
	const op = "store.StartMigration"
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		insert into migration_state
		  (from_name, from_model, from_table, to_name, to_model, to_table, chunks_total)
		values ($1,$2,$3,$4,$5,$6,$7) returning id`,
		m.FromName, m.FromModel, m.FromTable, m.ToName, m.ToModel, m.ToTable, m.ChunksTotal).Scan(&id)
	if err != nil {
		return uuid.Nil, cogerr.Ef(op, cogerr.Conflict, "could not start migration (already in progress?): %v", err)
	}
	return id, nil
}

// ActiveMigration returns the single in_progress migration, or NotFound.
func (s *Store) ActiveMigration(ctx context.Context) (Migration, error) {
	const op = "store.ActiveMigration"
	m, err := scanMigration(s.pool.QueryRow(ctx,
		`select `+migrationCols+` from migration_state where status = 'in_progress'`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, cogerr.Ef(op, cogerr.NotFound, "no migration in progress")
	}
	if err != nil {
		return Migration{}, cogerr.E(op, cogerr.Internal, err)
	}
	return m, nil
}

// LatestMigration returns the most recent migration row regardless of status
// (backs the status report after completion/rollback), or NotFound.
func (s *Store) LatestMigration(ctx context.Context) (Migration, error) {
	const op = "store.LatestMigration"
	m, err := scanMigration(s.pool.QueryRow(ctx,
		`select `+migrationCols+` from migration_state order by started_at desc limit 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, cogerr.Ef(op, cogerr.NotFound, "no migrations recorded")
	}
	if err != nil {
		return Migration{}, cogerr.E(op, cogerr.Internal, err)
	}
	return m, nil
}

// SetMigrationPaused persists the pause flag (survives daemon restarts).
func (s *Store) SetMigrationPaused(ctx context.Context, id uuid.UUID, paused bool) error {
	const op = "store.SetMigrationPaused"
	tag, err := s.pool.Exec(ctx,
		`update migration_state set paused = $2 where id = $1 and status = 'in_progress'`, id, paused)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if tag.RowsAffected() == 0 {
		return cogerr.Ef(op, cogerr.NotFound, "no in-progress migration %s", id)
	}
	return nil
}

// FinishMigration marks the terminal status (complete | rolled_back).
func (s *Store) FinishMigration(ctx context.Context, id uuid.UUID, status string) error {
	const op = "store.FinishMigration"
	if status != "complete" && status != "rolled_back" {
		return cogerr.Ef(op, cogerr.Validation, "bad terminal status %q", status)
	}
	tag, err := s.pool.Exec(ctx, `
		update migration_state set status = $2, finished_at = now()
		where id = $1 and status = 'in_progress'`, id, status)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if tag.RowsAffected() == 0 {
		return cogerr.Ef(op, cogerr.NotFound, "no in-progress migration %s", id)
	}
	return nil
}

// counterColumn maps a progress counter name to its column.
func counterColumn(op, counter string) (string, error) {
	switch counter {
	case "backfill":
		return "chunks_backfill", nil
	case "lazy":
		return "chunks_lazy", nil
	case "failed":
		return "chunks_failed", nil
	default:
		return "", cogerr.Ef(op, cogerr.Validation, "unknown counter %q", counter)
	}
}

// bumpMigrationCounterTx adds n to one of the progress counters, inside the
// caller's transaction.
func bumpMigrationCounterTx(ctx context.Context, tx pgxTx, id uuid.UUID, counter string, n int) error {
	const op = "store.bumpMigrationCounter"
	col, err := counterColumn(op, counter)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`update migration_state set %s = %s + $2 where id = $1`, col, col), id, n); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// RecordMigratedBatch writes a batch's embeddings and credits the progress
// counter for exactly what landed — in one transaction.
//
// The atomicity is the point, not an optimisation. These were previously two
// separate transactions, and a context cancellation arriving between them left
// the embeddings durably committed while the counter never saw them. Nothing
// recovers that: MissingChunkBatch and MissingCount both key on "no row in the
// destination table", so a resumed worker never revisits those chunks, and no
// code re-derives the counters from table state. The migration then completed
// with chunks_backfill + chunks_lazy permanently short of chunks_total — the
// completion invariant the schema documents, silently violated, and reported
// to users as a self-contradictory "800/800 done (768 backfill, 32 lazy)".
//
// Committing both together makes the shortfall unrepresentable: either the
// rows and the credit both land, or neither does and the batch is retried.
func (s *Store) RecordMigratedBatch(ctx context.Context, table string,
	vecs map[uuid.UUID][]float32, migrationID uuid.UUID, counter string) (int, error) {
	const op = "store.RecordMigratedBatch"
	if !tableNameRe.MatchString(table) {
		return 0, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	if _, err := counterColumn(op, counter); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	inserted, err := insertEmbeddingsIfAbsentTx(ctx, tx, table, vecs)
	if err != nil {
		return 0, err
	}
	if inserted > 0 {
		if err := bumpMigrationCounterTx(ctx, tx, migrationID, counter, inserted); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	return inserted, nil
}

// RecordMigrationError stores the most recent worker error for the report.
func (s *Store) RecordMigrationError(ctx context.Context, id uuid.UUID, msg string) error {
	const op = "store.RecordMigrationError"
	if _, err := s.pool.Exec(ctx, `update migration_state set last_error = $2 where id = $1`, id, msg); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// ChunkRef is one back-fill work item.
type ChunkRef struct {
	ID      uuid.UUID
	Content string
}

// MissingChunkBatch returns up to limit chunks that have no embedding in the
// given table yet — the anti-join means lazy-migrated chunks are naturally
// excluded, so the two paths never duplicate work.
func (s *Store) MissingChunkBatch(ctx context.Context, table string, limit int) ([]ChunkRef, error) {
	const op = "store.MissingChunkBatch"
	if !tableNameRe.MatchString(table) {
		return nil, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		select c.id, c.content from chunks c
		where not exists (select 1 from %s e where e.chunk_id = c.id)
		order by c.id
		limit $1`, table), limit)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out, err := scanChunkRefs(rows)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	return out, nil
}

// scanChunkRefs drains a chunk-id/content result set into ChunkRefs, closing
// rows on every path — the shared tail of every ChunkRef query below.
func scanChunkRefs(rows pgx.Rows) ([]ChunkRef, error) {
	defer rows.Close()
	var out []ChunkRef
	for rows.Next() {
		var c ChunkRef
		if err := rows.Scan(&c.ID, &c.Content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MissingCount reports how many chunks still lack an embedding in the table.
func (s *Store) MissingCount(ctx context.Context, table string) (int, error) {
	const op = "store.MissingCount"
	if !tableNameRe.MatchString(table) {
		return 0, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	var n int
	if err := s.pool.QueryRow(ctx, fmt.Sprintf(`
		select count(*) from chunks c
		where not exists (select 1 from %s e where e.chunk_id = c.id)`, table)).Scan(&n); err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	return n, nil
}

// ChunkRefsForNote returns a note's chunks in ordinal order — used to embed
// an already-indexed note's chunks into a specific table (test fixtures, and
// any future re-embed tooling).
func (s *Store) ChunkRefsForNote(ctx context.Context, path string) ([]ChunkRef, error) {
	const op = "store.ChunkRefsForNote"
	rows, err := s.pool.Query(ctx,
		`select id, content from chunks where note_path = $1 order by ordinal`, path)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out, err := scanChunkRefs(rows)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	return out, nil
}

// CountAllChunks is the corpus size a migration must eventually cover.
func (s *Store) CountAllChunks(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `select count(*) from chunks`).Scan(&n); err != nil {
		return 0, cogerr.E("store.CountAllChunks", cogerr.Internal, err)
	}
	return n, nil
}

// MissingAmong filters the given chunk ids down to those absent from the
// table — the lazy path's pre-check.
func (s *Store) MissingAmong(ctx context.Context, table string, ids []uuid.UUID) ([]ChunkRef, error) {
	const op = "store.MissingAmong"
	if !tableNameRe.MatchString(table) {
		return nil, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		select c.id, c.content from chunks c
		where c.id = any($1)
		  and not exists (select 1 from %s e where e.chunk_id = c.id)`, table), ids)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out, err := scanChunkRefs(rows)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	return out, nil
}

// SetActiveProvider flips the single active provider row (completion and
// rollback both land here).
func (s *Store) SetActiveProvider(ctx context.Context, name, model string) error {
	const op = "store.SetActiveProvider"
	tag, err := s.pool.Exec(ctx,
		`update embedding_providers set active = (name = $1 and model = $2)`, name, model)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if tag.RowsAffected() == 0 {
		return cogerr.Ef(op, cogerr.NotFound, "no provider rows to activate")
	}
	return nil
}

// DropProvider removes a retired provider's table and registry row — the
// deliberate, explicit prune. Safety (not active, not mid-migration) is the
// caller's check; this just executes.
func (s *Store) DropProvider(ctx context.Context, name, model string) error {
	const op = "store.DropProvider"
	var table string
	err := s.pool.QueryRow(ctx,
		`select table_name from embedding_providers where name = $1 and model = $2`, name, model).Scan(&table)
	if errors.Is(err, pgx.ErrNoRows) {
		return cogerr.Ef(op, cogerr.NotFound, "no provider %s/%s registered", name, model)
	}
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if !tableNameRe.MatchString(table) {
		return cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `drop table if exists `+table); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if _, err := tx.Exec(ctx,
		`delete from embedding_providers where name = $1 and model = $2`, name, model); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}
