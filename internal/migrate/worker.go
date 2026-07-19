package migrate

import (
	"context"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
)

const (
	// batchSize bounds one back-fill embed round trip.
	batchSize = 32
	// pollInterval is how often the worker looks for (or re-checks) work.
	pollInterval = 2 * time.Second
	// maxBackoff caps the rate-limit backoff between failed batches.
	maxBackoff = 2 * time.Minute
)

// Worker is the back-fill half: a daemon runner that continuously processes
// batches while a migration is in progress and unpaused. It guarantees
// eventual 100% coverage -- the precondition for flipping the active provider.
// Implements daemon.Runner.
type Worker struct {
	C *Coordinator
	// Poll overrides pollInterval in tests; zero means the default.
	Poll time.Duration
}

func (w *Worker) Run(ctx context.Context) error {
	poll := w.Poll
	if poll <= 0 {
		poll = pollInterval
	}
	backoff := poll
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		progressed, err := w.step(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			// Rate-limit aware: back off instead of failing the migration;
			// the error is recorded on the state row for the status report.
			w.C.log().Warn("backfill batch failed; backing off", "reason", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		case progressed:
			backoff = poll
			// Keep draining without waiting for the next tick.
			ticker.Reset(time.Nanosecond)
		default:
			backoff = poll
			ticker.Reset(poll)
		}
	}
}

// step processes at most one batch. Returns whether it made progress.
func (w *Worker) step(ctx context.Context) (bool, error) {
	m, err := w.C.Store.ActiveMigration(ctx)
	if err != nil {
		if cogerr.Is(err, cogerr.NotFound) {
			return false, nil // no migration: idle
		}
		return false, err
	}
	if m.Paused {
		return false, nil
	}

	batch, err := w.C.Store.MissingChunkBatch(ctx, m.ToTable, batchSize)
	if err != nil {
		return false, err
	}
	if len(batch) == 0 {
		// 100% coverage: flip active and finish. Old table stays until an
		// explicit prune.
		if err := w.C.Store.SetActiveProvider(ctx, m.ToName, m.ToModel); err != nil {
			return false, err
		}
		if err := w.C.Store.FinishMigration(ctx, m.ID, "complete"); err != nil {
			return false, err
		}
		w.C.log().Info("migration complete; provider flipped",
			"active", m.ToName+"/"+m.ToModel, "backfill", m.ChunksBackfill, "lazy", m.ChunksLazy)
		return true, nil
	}

	if err := w.C.embedBatch(ctx, m, batch, "backfill"); err != nil {
		// Transient failure: the batch stays missing and will be retried, so
		// only the error is recorded -- counters track chunks actually moved.
		if rerr := w.C.Store.RecordMigrationError(ctx, m.ID, err.Error()); rerr != nil {
			w.C.log().Warn("recording migration error failed", "reason", rerr)
		}
		return false, err
	}
	return true, nil
}
