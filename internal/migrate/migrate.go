// Package migrate is the zero-downtime embedding-provider migration: a
// background back-fill worker and a lazy touch-migration path, both reducing
// to "ensure this chunk exists in the new table" (upsert on the chunk_id
// primary key), so they are idempotent and never duplicate work. Retrieval
// stays fully available throughout because the query engine already fans out
// across every provisioned provider table — a chunk not yet migrated is
// found via the old table.
//
// The migration_state row is the coordination medium: the CLI writes it, the
// daemon's worker acts on it, and a daemon restart resumes an in-flight
// migration with no extra state.
package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/daemon"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/write"
)

// Factory builds a provider client from its registry identity. The daemon
// wires Ollama; tests wire deterministic stubs.
type Factory func(name, model string) (embed.Provider, error)

// Coordinator owns migration control (start/pause/resume/rollback/status)
// and the lazy path. The Worker (worker.go) owns the back-fill.
type Coordinator struct {
	Store   *store.Store
	Factory Factory
	Log     *slog.Logger
}

// ParseProviderRef splits "name/model" (e.g. "ollama/nomic-embed-text:v1.5").
func ParseProviderRef(ref string) (name, model string, err error) {
	name, model, ok := strings.Cut(ref, "/")
	if !ok || name == "" || model == "" {
		return "", "", cogerr.Ef("migrate.ParseProviderRef", cogerr.Validation,
			"provider ref %q must be <name>/<model>", ref)
	}
	return name, model, nil
}

// Plan is what a dry run reports.
type Plan struct {
	From, To    store.Provider
	ChunksTotal int
}

// Start validates both providers, provisions the target table (inactive), and
// inserts the in_progress row — all under the whole-KB migration lock. A dry
// run returns the plan and writes nothing.
func (c *Coordinator) Start(ctx context.Context, fromRef, toRef string, dryRun bool) (*Plan, error) {
	const op = "migrate.Start"
	release, err := c.Store.AcquireAdvisory(ctx, store.LockMigrate)
	if err != nil {
		return nil, err
	}
	defer release()

	fromName, fromModel, err := ParseProviderRef(fromRef)
	if err != nil {
		return nil, err
	}
	toName, toModel, err := ParseProviderRef(toRef)
	if err != nil {
		return nil, err
	}
	if fromRef == toRef {
		return nil, cogerr.Ef(op, cogerr.Validation, "from and to are the same provider")
	}
	if _, err := c.Store.ActiveMigration(ctx); err == nil {
		return nil, cogerr.Ef(op, cogerr.Conflict, "a migration is already in progress")
	}

	// The from-provider must be registered (it's what the corpus is embedded
	// under); typically it's the active one.
	from, err := c.findProvider(ctx, fromName, fromModel)
	if err != nil {
		return nil, err
	}

	// Resolve the to-provider's dimension (known-model shortcut or live probe)
	// and its would-be table.
	toClient, err := c.Factory(toName, toModel)
	if err != nil {
		return nil, err
	}
	dim, err := toClient.Dimension(ctx)
	if err != nil {
		return nil, cogerr.Ef(op, cogerr.Unavailable, "probing %s/%s: %v", toName, toModel, err)
	}
	toTable := embed.TableSlug(toName, toModel)

	// Total = every chunk in the corpus at start time (new writes dual-embed
	// and never depend on the migration paths).
	chunksTotal, err := c.countChunks(ctx)
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		From:        from,
		To:          store.Provider{Name: toName, Model: toModel, Dimension: dim, Table: toTable},
		ChunksTotal: chunksTotal,
	}
	if dryRun {
		return plan, nil
	}

	if err := c.Store.EnsureProvider(ctx, toName, toModel, toTable, dim, false); err != nil {
		return nil, err
	}
	if _, err := c.Store.StartMigration(ctx, store.Migration{
		FromName: fromName, FromModel: fromModel, FromTable: from.Table,
		ToName: toName, ToModel: toModel, ToTable: toTable,
		ChunksTotal: chunksTotal,
	}); err != nil {
		return nil, err
	}
	c.log().Info("migration started", "from", fromRef, "to", toRef, "chunks", chunksTotal)
	return plan, nil
}

// Pause parks the back-fill worker (persisted; survives restarts). The lazy
// path and dual-embedding keep running — they're write-path side effects.
func (c *Coordinator) Pause(ctx context.Context) error {
	m, err := c.Store.ActiveMigration(ctx)
	if err != nil {
		return err
	}
	return c.Store.SetMigrationPaused(ctx, m.ID, true)
}

// Resume unparks the worker.
func (c *Coordinator) Resume(ctx context.Context) error {
	m, err := c.Store.ActiveMigration(ctx)
	if err != nil {
		return err
	}
	return c.Store.SetMigrationPaused(ctx, m.ID, false)
}

// Rollback is immediate. Mid-migration the active provider never moved, so
// marking the row rolled_back stops the worker, the lazy path, and
// dual-embedding — behavior is old-provider-only at once. After completion it
// also flips active back. The half-migrated table stays in place so a later
// attempt resumes rather than restarts.
func (c *Coordinator) Rollback(ctx context.Context) error {
	const op = "migrate.Rollback"
	if m, err := c.Store.ActiveMigration(ctx); err == nil {
		return c.Store.FinishMigration(ctx, m.ID, "rolled_back")
	}
	m, err := c.Store.LatestMigration(ctx)
	if err != nil {
		return err
	}
	if m.Status != "complete" {
		return cogerr.Ef(op, cogerr.Validation, "nothing to roll back (latest migration is %s)", m.Status)
	}
	if err := c.Store.SetActiveProvider(ctx, m.FromName, m.FromModel); err != nil {
		return err
	}
	c.log().Info("migration rolled back post-completion", "active", m.FromName+"/"+m.FromModel)
	return nil
}

// Status is the progress report backing both the CLI and the MCP tool.
type Status struct {
	Migration store.Migration
	Missing   int
	ETA       time.Duration // zero when unknowable
}

func (s Status) String() string {
	m := s.Migration
	var b strings.Builder
	fmt.Fprintf(&b, "migration %s/%s -> %s/%s: %s", m.FromName, m.FromModel, m.ToName, m.ToModel, m.Status)
	if m.Paused {
		b.WriteString(" (paused)")
	}
	done := m.ChunksTotal - s.Missing
	fmt.Fprintf(&b, "\n  chunks: %d/%d done (%d backfill, %d lazy, %d failed)",
		done, m.ChunksTotal, m.ChunksBackfill, m.ChunksLazy, m.ChunksFailed)
	if s.ETA > 0 {
		fmt.Fprintf(&b, "\n  eta: ~%s", s.ETA.Round(time.Second))
	}
	if m.LastError != "" {
		fmt.Fprintf(&b, "\n  last error: %s", m.LastError)
	}
	return b.String()
}

// GetStatus reports the active migration, or the latest terminal one.
func (c *Coordinator) GetStatus(ctx context.Context) (*Status, error) {
	m, err := c.Store.ActiveMigration(ctx)
	if err != nil {
		if m, lerr := c.Store.LatestMigration(ctx); lerr == nil {
			return &Status{Migration: m}, nil
		}
		return nil, err
	}
	missing, err := c.Store.MissingCount(ctx, m.ToTable)
	if err != nil {
		return nil, err
	}
	st := &Status{Migration: m, Missing: missing}
	if done := m.ChunksTotal - missing; done > 0 && missing > 0 {
		elapsed := time.Since(m.StartedAt)
		st.ETA = time.Duration(float64(elapsed) / float64(done) * float64(missing))
	}
	return st, nil
}

// LazyEnsure is the touch-migration path: fire-and-forget, called with the
// chunk ids a query just served. Chunks already covered are filtered by the
// same anti-join the worker uses, so this never duplicates work.
func (c *Coordinator) LazyEnsure(ids []uuid.UUID) {
	go func() {
		defer daemon.RecoverPanic(c.log(), "migrate.LazyEnsure", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		m, err := c.Store.ActiveMigration(ctx)
		if err != nil {
			return // no migration: nothing to do
		}
		missing, err := c.Store.MissingAmong(ctx, m.ToTable, ids)
		if err != nil {
			c.log().Warn("lazy migration precheck failed", "reason", err)
			return
		}
		if len(missing) == 0 {
			return
		}
		if err := c.embedBatch(ctx, m, missing, "lazy"); err != nil {
			c.log().Warn("lazy migration batch failed", "reason", err)
		}
	}()
}

// embedBatch embeds one batch via the to-provider and inserts what isn't
// there yet, bumping the named counter by the rows that actually landed —
// so a chunk racing between the back-fill and the lazy path is counted
// exactly once toward the backfill+lazy == total invariant.
func (c *Coordinator) embedBatch(ctx context.Context, m store.Migration, batch []store.ChunkRef, counter string) error {
	provider, err := c.Factory(m.ToName, m.ToModel)
	if err != nil {
		return err
	}
	texts := make([]string, len(batch))
	for i, ch := range batch {
		texts[i] = ch.Content
	}
	vecs, err := provider.Embed(ctx, texts)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID][]float32, len(batch))
	for i, ch := range batch {
		byID[ch.ID] = vecs[i]
	}
	// One call, one transaction: the rows and the credit for them commit
	// together. Splitting these was the bug — a cancel between the write and
	// the counter bump left the embeddings committed and permanently
	// uncounted, because nothing revisits a chunk that already has a row.
	_, err = c.Store.RecordMigratedBatch(ctx, m.ToTable, byID, m.ID, counter)
	return err
}

// EmbedTargets reports where writes must embed right now: the active provider
// plus, during a migration, the in-progress target — new writes are born
// fully covered and never depend on either migration path. Wired as the
// indexer's TargetsFn.
func (c *Coordinator) EmbedTargets(ctx context.Context) ([]write.EmbedTarget, error) {
	active, err := c.Store.ActiveProvider(ctx)
	if err != nil {
		return nil, err
	}
	activeClient, err := c.Factory(active.Name, active.Model)
	if err != nil {
		return nil, err
	}
	targets := []write.EmbedTarget{{Provider: activeClient, Table: active.Table}}
	if m, err := c.Store.ActiveMigration(ctx); err == nil && m.ToTable != active.Table {
		toClient, err := c.Factory(m.ToName, m.ToModel)
		if err != nil {
			return nil, err
		}
		targets = append(targets, write.EmbedTarget{Provider: toClient, Table: m.ToTable})
	}
	return targets, nil
}

func (c *Coordinator) findProvider(ctx context.Context, name, model string) (store.Provider, error) {
	const op = "migrate.findProvider"
	ps, err := c.Store.Providers(ctx)
	if err != nil {
		return store.Provider{}, err
	}
	for _, p := range ps {
		if p.Name == name && p.Model == model {
			return p, nil
		}
	}
	return store.Provider{}, cogerr.Ef(op, cogerr.NotFound, "provider %s/%s is not registered", name, model)
}

func (c *Coordinator) countChunks(ctx context.Context) (int, error) {
	return c.Store.CountAllChunks(ctx)
}

func (c *Coordinator) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
