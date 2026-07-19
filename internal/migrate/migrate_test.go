package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
)

const (
	fromRef = "stub/model-a"
	toRef   = "stub2/model-b"
)

type harness struct {
	s         *store.Store
	coord     *Coordinator
	engine    *query.Engine
	fromTable string
	toTable   string
	ctx       context.Context
}

// newHarness seeds a corpus of chunkCount chunks fully embedded under the
// from-provider (the pre-migration steady state) and wires a stub factory.
func newHarness(t *testing.T, chunkCount int) *harness {
	t.Helper()
	s, _ := storetest.New(t)
	ctx := context.Background()

	stubA := embedtest.New()
	stubA.ModelName = "model-a"
	stubB := embedtest.New()
	stubB.ModelName = "model-b"
	factory := func(name, model string) (embed.Provider, error) {
		switch name {
		case "stub":
			return stubA, nil
		case "stub2":
			return stubB, nil
		}
		return nil, fmt.Errorf("unknown provider %q", name)
	}

	fromTable := embed.TableSlug("stub", "model-a")
	if err := s.EnsureProvider(ctx, "stub", "model-a", fromTable, stubA.Dim, true); err != nil {
		t.Fatal(err)
	}

	// Corpus: notes of 100 chunks each, embedded under the from-provider.
	perNote := 100
	if chunkCount < perNote {
		perNote = chunkCount
	}
	for noteIdx := 0; noteIdx*perNote < chunkCount; noteIdx++ {
		path := fmt.Sprintf("entries/corpus-%03d.md", noteIdx)
		n := store.Note{
			Path: path, ID: uuid.New(), Category: "entry", Status: "active",
			Created: time.Now().UTC(), Updated: time.Now().UTC(),
			Frontmatter: map[string]any{"id": "x"}, Content: "corpus note",
			Mtime: time.Now().UTC(), Size: 1, Blake3: path,
		}
		if err := s.UpsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
		count := min(perNote, chunkCount-noteIdx*perNote)
		chunks := make([]store.Chunk, count)
		texts := make([]string, count)
		for i := range chunks {
			content := fmt.Sprintf("chunk %d of note %d speaking about topic %d", i, noteIdx, (noteIdx*perNote+i)%17)
			chunks[i] = store.Chunk{Ordinal: i, Content: content, ContentHash: content}
			texts[i] = content
		}
		if err := s.ReplaceChunks(ctx, path, chunks); err != nil {
			t.Fatal(err)
		}
		// Embed under the from-provider.
		ids := chunkIDs(t, s, ctx, path)
		vecsList, err := stubA.Embed(ctx, texts)
		if err != nil {
			t.Fatal(err)
		}
		vecs := make(map[uuid.UUID][]float32, count)
		for i, id := range ids {
			vecs[id] = vecsList[i]
		}
		if err := s.UpsertEmbeddings(ctx, fromTable, vecs); err != nil {
			t.Fatal(err)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	coord := &Coordinator{Store: s, Factory: factory, Log: log}
	engine := &query.Engine{Store: s, Factory: factory, Lazy: coord.LazyEnsure}
	return &harness{
		s: s, coord: coord, engine: engine,
		fromTable: fromTable, toTable: embed.TableSlug("stub2", "model-b"),
		ctx: ctx,
	}
}

func chunkIDs(t *testing.T, s *store.Store, ctx context.Context, path string) []uuid.UUID {
	t.Helper()
	// Reuse MissingAmong's shape via a direct batch read: everything is
	// missing from a table that doesn't exist yet, so read via the batch API
	// against the from table before embedding... simplest is the dedicated
	// helper below.
	refs, err := s.ChunkRefsForNote(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

func (h *harness) waitDone(t *testing.T, deadline time.Duration) store.Migration {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if _, err := h.s.ActiveMigration(h.ctx); cogerr.Is(err, cogerr.NotFound) {
			m, err := h.s.LatestMigration(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			return m
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("migration did not finish within deadline")
	return store.Migration{}
}

// TestZeroDowntimeUnderLoad is the M4 claim in test form: a 5k-chunk corpus
// migrates between providers while goroutines hammer the query path -- zero
// queries return empty results at any point.
func TestZeroDowntimeUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	h := newHarness(t, 5000)

	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}

	workerCtx, stopWorker := context.WithCancel(h.ctx)
	defer stopWorker()
	go func() { _ = (&Worker{C: h.coord, Poll: 5 * time.Millisecond}).Run(workerCtx) }()

	var empties, queries atomic.Int64
	loadCtx, stopLoad := context.WithCancel(h.ctx)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			i := 0
			for loadCtx.Err() == nil {
				res, err := h.engine.Run(h.ctx, fmt.Sprintf("topic %d from loader %d", i%17, g), query.Options{})
				if err == nil {
					queries.Add(1)
					if len(res) == 0 {
						empties.Add(1)
					}
				}
				i++
			}
		}(g)
	}

	m := h.waitDone(t, 120*time.Second)
	stopLoad()
	wg.Wait()

	if m.Status != "complete" {
		t.Fatalf("migration status = %s", m.Status)
	}
	if empties.Load() != 0 {
		t.Fatalf("%d of %d queries returned empty results during migration -- the zero-downtime claim failed",
			empties.Load(), queries.Load())
	}
	if queries.Load() == 0 {
		t.Fatal("load generator never completed a query; the test proved nothing")
	}

	// Coverage and accounting: every chunk landed exactly once.
	missing, err := h.s.MissingCount(h.ctx, h.toTable)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("missing after completion = %d", missing)
	}
	if got := m.ChunksBackfill + m.ChunksLazy; got != m.ChunksTotal {
		t.Fatalf("backfill(%d) + lazy(%d) = %d, want total %d",
			m.ChunksBackfill, m.ChunksLazy, got, m.ChunksTotal)
	}
	active, err := h.s.ActiveProvider(h.ctx)
	if err != nil || active.Name != "stub2" {
		t.Fatalf("active after completion = %+v (%v)", active, err)
	}
	t.Logf("load: %d queries, backfill %d, lazy %d", queries.Load(), m.ChunksBackfill, m.ChunksLazy)
}

// TestPauseResumeConverges -- pause parks the back-fill (missing count stops
// moving), resume completes, and the counters account for every chunk.
func TestPauseResumeConverges(t *testing.T) {
	h := newHarness(t, 500)
	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(h.ctx)
	defer stopWorker()
	go func() { _ = (&Worker{C: h.coord, Poll: 5 * time.Millisecond}).Run(workerCtx) }()

	// Let it make some progress, then pause.
	waitFor(t, 30*time.Second, func() bool {
		n, _ := h.s.MissingCount(h.ctx, h.toTable)
		return n < 500
	})
	if err := h.coord.Pause(h.ctx); err != nil {
		t.Fatal(err)
	}
	// Parked: missing count settles and stays put across several polls.
	var settled int
	waitFor(t, 10*time.Second, func() bool {
		a, _ := h.s.MissingCount(h.ctx, h.toTable)
		time.Sleep(100 * time.Millisecond)
		b, _ := h.s.MissingCount(h.ctx, h.toTable)
		settled = b
		return a == b
	})
	time.Sleep(200 * time.Millisecond)
	if n, _ := h.s.MissingCount(h.ctx, h.toTable); n != settled {
		t.Fatalf("paused worker still progressing: %d -> %d", settled, n)
	}
	if settled == 0 {
		t.Fatal("migration finished before pause could be observed; corpus too small for this test")
	}

	if err := h.coord.Resume(h.ctx); err != nil {
		t.Fatal(err)
	}
	m := h.waitDone(t, 60*time.Second)
	if m.Status != "complete" {
		t.Fatalf("status = %s", m.Status)
	}
	if m.ChunksBackfill+m.ChunksLazy != m.ChunksTotal {
		t.Fatalf("backfill(%d)+lazy(%d) != total(%d)", m.ChunksBackfill, m.ChunksLazy, m.ChunksTotal)
	}
}

// TestKillMidBatchResumes -- cancelling the worker and starting a fresh one
// resumes with no duplicate embeddings and exact accounting.
func TestKillMidBatchResumes(t *testing.T) {
	h := newHarness(t, 800)
	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}

	ctx1, kill := context.WithCancel(h.ctx)
	done1 := make(chan struct{})
	go func() { _ = (&Worker{C: h.coord, Poll: time.Millisecond}).Run(ctx1); close(done1) }()
	waitFor(t, 30*time.Second, func() bool {
		n, _ := h.s.MissingCount(h.ctx, h.toTable)
		return n < 800 && n > 0
	})
	kill()
	<-done1

	// Fresh worker resumes from persisted state.
	ctx2, stop2 := context.WithCancel(h.ctx)
	defer stop2()
	go func() { _ = (&Worker{C: h.coord, Poll: time.Millisecond}).Run(ctx2) }()
	m := h.waitDone(t, 60*time.Second)

	if m.Status != "complete" {
		t.Fatalf("status = %s", m.Status)
	}
	rows, err := h.s.CountEmbeddings(h.ctx, h.toTable)
	if err != nil {
		t.Fatal(err)
	}
	if rows != m.ChunksTotal {
		t.Fatalf("new table rows = %d, want %d (duplicates or gaps)", rows, m.ChunksTotal)
	}
	if sum := m.ChunksBackfill + m.ChunksLazy; sum != m.ChunksTotal {
		// Name the direction. This message used to say only "double-counted",
		// which sent the first investigation the wrong way: the observed
		// failure was an *under*-count, from a batch whose embeddings
		// committed while the credit for them did not. Both directions break
		// the completion invariant and they have opposite causes.
		direction := "under-counted (work committed without credit)"
		if sum > m.ChunksTotal {
			direction = "double-counted (credited more than once)"
		}
		t.Fatalf("backfill(%d)+lazy(%d) = %d != total(%d) -- chunks were %s",
			m.ChunksBackfill, m.ChunksLazy, sum, m.ChunksTotal, direction)
	}
}

// TestLazyMigratesAheadOfBackfill -- with the worker never running (paused
// migration, no worker goroutine), a query's hits still migrate via the lazy
// path: hot memory ahead of batch order.
func TestLazyMigratesAheadOfBackfill(t *testing.T) {
	h := newHarness(t, 200)
	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}
	if err := h.coord.Pause(h.ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.Run(h.ctx, "topic 3 anything", query.Options{TopK: 8}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		n, _ := h.s.CountEmbeddings(h.ctx, h.toTable)
		return n > 0
	})
	m, err := h.s.ActiveMigration(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		m, _ = h.s.ActiveMigration(h.ctx)
		return m.ChunksLazy > 0
	})
	if m.ChunksBackfill != 0 {
		t.Fatalf("backfill = %d with no worker running", m.ChunksBackfill)
	}
}

// TestRollbackMidMigration -- immediate: the active provider never moved, the
// worker goes idle, and writes stop dual-embedding.
func TestRollbackMidMigration(t *testing.T) {
	h := newHarness(t, 100)
	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}
	targets, err := h.coord.EmbedTargets(h.ctx)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets during migration = %d (%v), want 2 (dual-embed)", len(targets), err)
	}

	if err := h.coord.Rollback(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.ActiveMigration(h.ctx); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("migration still active after rollback: %v", err)
	}
	active, err := h.s.ActiveProvider(h.ctx)
	if err != nil || active.Name != "stub" {
		t.Fatalf("active after rollback = %+v (%v), want the from-provider", active, err)
	}
	targets, err = h.coord.EmbedTargets(h.ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets after rollback = %d (%v), want 1 (dual-embed stopped)", len(targets), err)
	}
	// A parked worker finds nothing to do.
	w := &Worker{C: h.coord}
	progressed, err := w.step(h.ctx)
	if err != nil || progressed {
		t.Fatalf("worker acted after rollback: progressed=%v err=%v", progressed, err)
	}
}

// TestRollbackAfterCompletion flips the active provider back.
func TestRollbackAfterCompletion(t *testing.T) {
	h := newHarness(t, 64)
	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(h.ctx)
	defer stop()
	go func() { _ = (&Worker{C: h.coord, Poll: time.Millisecond}).Run(ctx) }()
	m := h.waitDone(t, 30*time.Second)
	if m.Status != "complete" {
		t.Fatalf("status = %s", m.Status)
	}
	if active, _ := h.s.ActiveProvider(h.ctx); active.Name != "stub2" {
		t.Fatalf("active = %s, want stub2", active.Name)
	}
	if err := h.coord.Rollback(h.ctx); err != nil {
		t.Fatal(err)
	}
	if active, _ := h.s.ActiveProvider(h.ctx); active.Name != "stub" {
		t.Fatalf("active after rollback = %s, want stub", active.Name)
	}
}

// TestDryRunWritesNothing -- plan only: no state row, no target table.
func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t, 50)
	plan, err := h.coord.Start(h.ctx, fromRef, toRef, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ChunksTotal != 50 || plan.To.Dimension != 8 {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := h.s.ActiveMigration(h.ctx); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatal("dry run created a migration row")
	}
	ps, err := h.s.Providers(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == "stub2" {
			t.Fatal("dry run provisioned the target provider")
		}
	}
}

// TestConcurrentStartConflicts -- a second start while one is in progress is
// an explicit Conflict, not a race.
func TestConcurrentStartConflicts(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.coord.Start(h.ctx, fromRef, toRef, false); err != nil {
		t.Fatal(err)
	}
	_, err := h.coord.Start(h.ctx, fromRef, toRef, false)
	if !cogerr.Is(err, cogerr.Conflict) {
		t.Fatalf("second start = %v, want Conflict", err)
	}
}

func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
