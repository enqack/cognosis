package watch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

// gate is an embed.Provider whose Embed blocks until released -- an embedding
// call frozen in flight. It honours the context it is handed, exactly like the
// real Ollama client: if that context dies first, the call fails. Which
// context the watcher hands it is therefore the behavior under test.
type gate struct {
	*embedtest.Stub
	arrived chan struct{} // closed when Embed is first entered
	release chan struct{} // closing it lets Embed proceed
	once    sync.Once
}

func newGate() *gate {
	return &gate{
		Stub:    embedtest.New(),
		arrived: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (g *gate) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	g.once.Do(func() { close(g.arrived) })
	// Block until released, then honour cancellation -- net/http fails a
	// request whose context died even if the response already arrived. Checking
	// after the block (rather than a two-arm select) keeps the test
	// deterministic: with cancel() sequenced before close(release), a select
	// with both arms ready would pick one at random.
	<-g.release
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return g.Stub.Embed(ctx, texts)
}

// TestInFlightIndexSurvivesShutdown pins the shutdown contract the daemon's
// runner drain relies on: a hand-edit whose index (embed call included) is in
// flight when shutdown cancels the watcher still lands -- in the store and in
// vault history -- rather than being aborted and dropped until the next boot
// reconciliation. Before watch.graceful existed, cancellation propagated into
// the write and killed the embed call the drain was waiting for.
func TestInFlightIndexSurvivesShutdown(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	g := newGate()
	if err := s.EnsureProvider(ctx, g.Name(), g.Model(),
		embed.TableSlug(g.Name(), g.Model()), g.Dim, true); err != nil {
		t.Fatal(err)
	}
	w.MakeIndexer = func(s *store.Store) *write.Indexer {
		return &write.Indexer{Store: s, Provider: g, Table: embed.TableSlug(g.Name(), g.Model())}
	}
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()
	time.Sleep(200 * time.Millisecond) // let the watcher arm

	writeEntry(t, root, "entries/inflight.md", entryContent(uuid.Must(uuid.NewV7()).String()))

	select {
	case <-g.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("embed call never started; the event was not picked up")
	}

	// Shutdown lands while the embed call is in flight; then the call is
	// allowed to complete, as a real HTTP response would arrive after SIGTERM.
	cancel()
	close(g.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watcher exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not exit after cancellation")
	}

	if _, err := s.GetNote(ctx, "entries/inflight.md"); err != nil {
		t.Fatalf("in-flight index was dropped by shutdown: %v", err)
	}
	lines, err := vault.NewHistory(root).Log(ctx, "entries/inflight.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("in-flight edit was indexed but never committed to vault history")
	}
}
