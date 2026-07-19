package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Per-provider embedding tables. pgvector requires a fixed dimension per
// column and vectors from different models are never comparable, so each
// provider/model pair gets its own table + HNSW index, provisioned at runtime
// once the dimension is known and registered in embedding_providers.

// Provider is a row in embedding_providers.
type Provider struct {
	Name      string
	Model     string
	Dimension int
	Table     string
	Active    bool
	CreatedAt time.Time
}

var tableNameRe = regexp.MustCompile(`^embeddings_[a-z0-9_]+$`)

// EnsureProvider provisions the table/index for a provider+model (idempotent)
// and registers it. makeActive flips it to the single active provider.
func (s *Store) EnsureProvider(ctx context.Context, name, model, table string, dim int, makeActive bool) error {
	const op = "store.EnsureProvider"
	if !tableNameRe.MatchString(table) {
		return cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	if dim <= 0 {
		return cogerr.Ef(op, cogerr.Validation, "bad dimension %d", dim)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Table names can't be bound parameters; the regexp above is the guard.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		create table if not exists %s (
		  chunk_id  uuid primary key references chunks(id) on delete cascade,
		  embedding vector(%d) not null
		)`, table, dim)); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`create index if not exists %s_hnsw_idx on %s using hnsw (embedding vector_cosine_ops)`,
		table, table)); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if _, err := tx.Exec(ctx, `
		insert into embedding_providers (name, model, dimension, table_name, active)
		values ($1,$2,$3,$4,false)
		on conflict (name, model) do update set dimension = excluded.dimension`,
		name, model, dim, table); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if makeActive {
		if _, err := tx.Exec(ctx, `update embedding_providers set active = (name = $1 and model = $2)`,
			name, model); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// Providers lists every registered provider, active first.
func (s *Store) Providers(ctx context.Context) ([]Provider, error) {
	const op = "store.Providers"
	rows, err := s.pool.Query(ctx, `
		select name, model, dimension, table_name, active, created_at
		from embedding_providers order by active desc, created_at`)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Name, &p.Model, &p.Dimension, &p.Table, &p.Active, &p.CreatedAt); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, p)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// ActiveProvider returns the single active provider registration.
func (s *Store) ActiveProvider(ctx context.Context) (Provider, error) {
	const op = "store.ActiveProvider"
	ps, err := s.Providers(ctx)
	if err != nil {
		return Provider{}, err
	}
	for _, p := range ps {
		if p.Active {
			return p, nil
		}
	}
	return Provider{}, cogerr.Ef(op, cogerr.NotFound, "no active embedding provider registered")
}

// UpsertEmbeddings writes chunk vectors into a provider table.
func (s *Store) UpsertEmbeddings(ctx context.Context, table string, vecs map[uuid.UUID][]float32) error {
	const op = "store.UpsertEmbeddings"
	if !tableNameRe.MatchString(table) {
		return cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := upsertEmbeddingsTx(ctx, tx, table, vecs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// insertEmbeddingsIfAbsentTx inserts vectors without overwriting existing rows
// and reports how many actually landed -- the migration paths' primitive, so
// a chunk racing between back-fill and lazy migration is counted exactly once
// (whoever gets there first wins; the loser's insert is a no-op).
//
// Transaction-scoped on purpose: the count it returns is what the migration's
// progress counter is bumped by, and the two must commit together or not at
// all. See RecordMigratedBatch.
func insertEmbeddingsIfAbsentTx(ctx context.Context, tx pgxTx, table string,
	vecs map[uuid.UUID][]float32) (int, error) {
	const op = "store.insertEmbeddingsIfAbsent"
	sql := fmt.Sprintf(`
		insert into %s (chunk_id, embedding) values ($1, $2)
		on conflict (chunk_id) do nothing`, table)
	inserted := 0
	for id, v := range vecs {
		tag, err := tx.Exec(ctx, sql, id, pgvector.NewVector(v))
		if err != nil {
			return 0, cogerr.E(op, cogerr.Internal, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// CountEmbeddings reports the row count of one provider table -- cheap
// introspection backing tests and migration progress reporting.
func (s *Store) CountEmbeddings(ctx context.Context, table string) (int, error) {
	const op = "store.CountEmbeddings"
	if !tableNameRe.MatchString(table) {
		return 0, cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `select count(*) from `+table).Scan(&n); err != nil {
		return 0, cogerr.E(op, cogerr.Internal, err)
	}
	return n, nil
}

func upsertEmbeddingsTx(ctx context.Context, tx pgxTx, table string, vecs map[uuid.UUID][]float32) error {
	const op = "store.upsertEmbeddings"
	sql := fmt.Sprintf(`
		insert into %s (chunk_id, embedding) values ($1, $2)
		on conflict (chunk_id) do update set embedding = excluded.embedding`, table)
	for id, v := range vecs {
		if _, err := tx.Exec(ctx, sql, id, pgvector.NewVector(v)); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}
	return nil
}
