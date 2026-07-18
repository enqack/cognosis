package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests exist because the failure they guard could not be reproduced on
// demand. TestKillMidBatchResumes (internal/migrate) only failed under
// full-suite load and passed every time in isolation, which made "it passes
// now" worthless as evidence. They drive the vulnerable window directly
// instead of waiting for the scheduler to find it.

// seedMigration returns a store with a provider table and an in_progress
// migration row, plus a batch of vectors ready to record.
func seedMigration(t *testing.T, chunks int) (*Store, context.Context, uuid.UUID, string, map[uuid.UUID][]float32) {
	t.Helper()
	s, ctx := testStore(t)

	const table = "embeddings_atomic_test"
	if err := s.EnsureProvider(ctx, "atomic", "test", table, 4, true); err != nil {
		t.Fatal(err)
	}
	id, err := s.StartMigration(ctx, Migration{
		FromName: "a", FromModel: "m1", FromTable: "embeddings_a_m1",
		ToName: "atomic", ToModel: "test", ToTable: table,
		ChunksTotal: chunks,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The embedding rows reference chunks(id), so real chunks must exist.
	if _, err := s.pool.Exec(ctx, `
		insert into notes (path, id, category, created, updated, frontmatter, content, mtime, size, blake3_hash)
		values ('n/atomic.md', $1, 'entry', now(), now(), '{}'::jsonb, '', now(), 0, '')`,
		uuid.New()); err != nil {
		t.Fatal(err)
	}
	vecs := map[uuid.UUID][]float32{}
	for i := range chunks {
		var cid uuid.UUID
		if err := s.pool.QueryRow(ctx, `
			insert into chunks (note_path, ordinal, content, content_hash)
			values ('n/atomic.md', $1, 'chunk', $2) returning id`,
			i, fmt.Sprintf("h%d", i)).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		vecs[cid] = []float32{float32(i), 0, 0, 1}
	}
	return s, ctx, id, table, vecs
}

// counterAndRows reads the two numbers that must agree. It reads the counter
// via LatestMigration rather than by id: these tests seed exactly one
// migration, and going through the same accessor the CLI and MCP status tools
// use keeps the assertion honest about what a user would see.
func counterAndRows(t *testing.T, ctx context.Context, s *Store, table string) (int, int) {
	t.Helper()
	m, err := s.LatestMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.CountEmbeddings(ctx, table)
	if err != nil {
		t.Fatal(err)
	}
	return m.ChunksBackfill, rows
}

// TestRecordMigratedBatchAtomicUnderCancel is the real guard. It cancels the
// context at a sweep of delays spanning the write, and after each attempt
// asserts the invariant the migration's completion check depends on: the
// progress counter equals the number of embedding rows that actually landed.
//
// Under the previous two-transaction implementation a cancel between the
// insert committing and the counter bump left rows > counter permanently —
// nothing revisits a chunk that already has a row, so the shortfall was not
// recoverable. Here either both land or neither does.
func TestRecordMigratedBatchAtomicUnderCancel(t *testing.T) {
	for _, delay := range []time.Duration{
		0, 50 * time.Microsecond, 100 * time.Microsecond, 250 * time.Microsecond,
		500 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond,
	} {
		s, ctx, id, table, vecs := seedMigration(t, 16)

		cctx, cancel := context.WithCancel(ctx)
		go func() {
			time.Sleep(delay)
			cancel()
		}()
		inserted, err := s.RecordMigratedBatch(cctx, table, vecs, id, "backfill")
		cancel()

		// Read back on the uncancelled context.
		counter, rows := counterAndRows(t, ctx, s, table)

		// The invariant, and the only one that matters: whatever landed is
		// credited. Not "nothing landed on error" — a COMMIT can succeed on
		// the server while the client sees context.Canceled, and the client
		// cannot distinguish that from a commit that never arrived. Both
		// outcomes are safe precisely *because* the two writes are atomic: if
		// the commit landed, the retry finds those chunks already present,
		// inserts 0 and credits 0; if it did not, the retry redoes both.
		// Asserting rows==0 on error would be asserting something Postgres
		// does not promise.
		if counter != rows {
			t.Errorf("delay %v: counter=%d but %d rows landed — the progress counter and the "+
				"destination table disagree, which is exactly the state that makes a migration "+
				"report itself complete while chunks_backfill+chunks_lazy < chunks_total",
				delay, counter, rows)
		}
		if err == nil && inserted != rows {
			t.Errorf("delay %v: reported %d inserted but %d rows landed", delay, inserted, rows)
		}
		if err == nil && inserted != len(vecs) {
			t.Errorf("delay %v: succeeded with %d of %d inserted — a partial success would mean "+
				"the batch is neither complete nor retryable", delay, inserted, len(vecs))
		}
	}
}

// A context cancelled before the call must write nothing at all — the
// deterministic end of the same invariant.
func TestRecordMigratedBatchPreCancelledWritesNothing(t *testing.T) {
	s, ctx, id, table, vecs := seedMigration(t, 8)

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := s.RecordMigratedBatch(cctx, table, vecs, id, "backfill"); err == nil {
		t.Fatal("expected an error from a pre-cancelled context")
	}
	counter, rows := counterAndRows(t, ctx, s, table)
	if rows != 0 || counter != 0 {
		t.Errorf("pre-cancelled call left state behind: counter=%d rows=%d", counter, rows)
	}
}

// The happy path still credits exactly what landed, including the re-run case
// where every row already exists and nothing new should be counted.
func TestRecordMigratedBatchCreditsOnlyWhatLanded(t *testing.T) {
	s, ctx, id, table, vecs := seedMigration(t, 8)

	inserted, err := s.RecordMigratedBatch(ctx, table, vecs, id, "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if inserted != len(vecs) {
		t.Errorf("first pass inserted %d, want %d", inserted, len(vecs))
	}
	counter, rows := counterAndRows(t, ctx, s, table)
	if counter != len(vecs) || rows != len(vecs) {
		t.Errorf("after first pass: counter=%d rows=%d, want %d/%d", counter, rows, len(vecs), len(vecs))
	}

	// Re-recording the same batch must be a no-op on both sides: this is the
	// backfill/lazy race, where whoever gets there first wins and the loser
	// must not double-count.
	again, err := s.RecordMigratedBatch(ctx, table, vecs, id, "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-recording an already-migrated batch inserted %d, want 0", again)
	}
	counter2, rows2 := counterAndRows(t, ctx, s, table)
	if counter2 != counter || rows2 != rows {
		t.Errorf("re-record changed state: counter %d→%d rows %d→%d", counter, counter2, rows, rows2)
	}
}
