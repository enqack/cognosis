package watch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

func testWatcher(t *testing.T) (*Watcher, *store.Store, string, context.Context) {
	t.Helper()
	s, _ := storetest.New(t)
	root := t.TempDir()
	for _, d := range []string{"entries", "notes", "reflections", "archive"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{KBPath: root, ReconcileSweepInterval: time.Hour}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := New(cfg, log)
	ctx := context.Background()
	if err := vault.NewHistory(root).EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	return w, s, root, ctx
}

func entryContent(id string) string {
	return `---
id: ` + id + `
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Raw capture body.
`
}

func writeEntry(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBootFastPathZeroHashes — the 1b exit criterion: a second boot against an
// unchanged vault computes zero hashes (counter assertion, not timing). Vault
// size is 1k files.
func TestBootFastPathZeroHashes(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	for i := range 1000 {
		writeEntry(t, root, fmt.Sprintf("entries/e%04d.md", i), entryContent(uuid.Must(uuid.NewV7()).String()))
	}
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := w.HashCount.Load(); got != 1000 {
		t.Fatalf("first boot hashed %d files, want 1000", got)
	}

	w.HashCount.Store(0)
	if err := w.reconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := w.HashCount.Load(); got != 0 {
		t.Fatalf("unchanged vault hashed %d files, want 0", got)
	}
}

// TestSweepCatchesMtimePreservingEdit — an edit that preserves size and mtime
// slips the fast path; the forced-hash sweep catches it.
func TestSweepCatchesMtimePreservingEdit(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	id := uuid.Must(uuid.NewV7()).String()
	p := writeEntry(t, root, "entries/sneaky.md", entryContent(id)+"original tail\n")
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// Same byte length, same mtime restored afterwards.
	if err := os.WriteFile(p, []byte(entryContent(id)+"tampered tail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	w.HashCount.Store(0)
	if err := w.reconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := w.HashCount.Load(); got != 0 {
		t.Fatalf("fast path hashed %d files for an mtime-preserving edit, want 0 (that's the point of the sweep)", got)
	}
	n, err := s.GetNote(ctx, "entries/sneaky.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.Content != "Raw capture body.\noriginal tail\n" {
		t.Fatalf("fast path should have missed the edit, content = %q", n.Content)
	}

	// The sweep (forced hash path) catches it.
	if err := w.reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	n, err = s.GetNote(ctx, "entries/sneaky.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.Content != "Raw capture body.\ntampered tail\n" {
		t.Fatalf("sweep missed the edit, content = %q", n.Content)
	}
}

// TestBrokenHandEditIsolated — invalid frontmatter is logged and not indexed;
// the previous DB state survives.
func TestBrokenHandEditIsolated(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	id := uuid.Must(uuid.NewV7()).String()
	p := writeEntry(t, root, "entries/fragile.md", entryContent(id))
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetNote(ctx, "entries/fragile.md")
	if err != nil {
		t.Fatal(err)
	}

	// Hand-edit that breaks the contract (drops required fields).
	if err := os.WriteFile(p, []byte("---\nid: not-even-a-uuid\n---\nbroken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.reconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetNote(ctx, "entries/fragile.md")
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != before.Content || after.ID != before.ID {
		t.Fatalf("broken edit leaked into the index: %+v", after)
	}
}

// TestDeletionReconciled — a file removed while the daemon was down disappears
// from the index on boot reconciliation.
func TestDeletionReconciled(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	p := writeEntry(t, root, "entries/gone.md", entryContent(uuid.Must(uuid.NewV7()).String()))
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := w.reconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, "entries/gone.md"); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("deleted file still indexed: %v", err)
	}
}

// TestDriftCommittedToHistory — confirmed drift lands as a history commit
// in the vault history repo.
func TestDriftCommittedToHistory(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	writeEntry(t, root, "entries/tracked.md", entryContent(uuid.Must(uuid.NewV7()).String()))
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	lines, err := vault.NewHistory(root).Log(ctx, "entries/tracked.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("drift was not committed to the history repo")
	}
}

// TestWatcherConvergence — live events converge the index within a deadline
// (never event-count assertions; fsnotify coalescing varies by platform).
func TestWatcherConvergence(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	// Give the watcher a moment to arm before generating events.
	time.Sleep(200 * time.Millisecond)

	p := writeEntry(t, root, "entries/live.md", entryContent(uuid.Must(uuid.NewV7()).String()))
	waitFor("create to be indexed", func() bool {
		_, err := s.GetNote(ctx, "entries/live.md")
		return err == nil
	})

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	waitFor("delete to be removed", func() bool {
		_, err := s.GetNote(ctx, "entries/live.md")
		return cogerr.Is(err, cogerr.NotFound)
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watcher exit: %v", err)
	}
}

// TestSuppressedPathIgnored — events for a path Cognosis is writing are
// dropped (write-conflict handling).
func TestSuppressedPathIgnored(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()
	time.Sleep(200 * time.Millisecond)

	w.Suppress("entries/mine.md")
	writeEntry(t, root, "entries/mine.md", entryContent(uuid.Must(uuid.NewV7()).String()))
	time.Sleep(500 * time.Millisecond) // would have been indexed by now if not suppressed
	if _, err := s.GetNote(ctx, "entries/mine.md"); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("suppressed path was indexed anyway: %v", err)
	}
	w.Unsuppress("entries/mine.md")
}

// TestBatchIndexResolvesLinksRegardlessOfOrder pins the graph against index
// order. Link resolution matches notes already in the index, so in a batch a
// note whose target is indexed later loses that edge — and nothing repairs it,
// because reconciliation confirms drift by content hash and an unchanged file
// is skipped forever. A rebuild after dropping the schema therefore silently
// produced a partial graph.
//
// "zzz" sorts after "aaa", so aaa (the source) is walked and indexed first,
// which is exactly the losing order.
func TestBatchIndexResolvesLinksRegardlessOfOrder(t *testing.T) {
	w, s, root, ctx := testWatcher(t)

	target := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/zzz-target.md", entryContent(target))

	src := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/aaa-source.md", `---
id: `+src+`
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Refers to [[zzz-target]] in the body.
`)

	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}

	srcID := uuid.MustParse(src)
	dsts, err := s.LinkDsts(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsts) != 1 || dsts[0] != uuid.MustParse(target) {
		t.Fatalf("link from aaa-source to zzz-target missing after batch index: got %v", dsts)
	}
}

// TestLaterArrivalRepairsDanglingLink covers the half of the ordering problem
// relinkBatch cannot: the referrer is not in the batch at all.
//
// A references B, B does not exist yet, so A's edge is dropped. When B lands
// in a *later* run, A is unchanged — drift detection confirms by content hash
// and skips it forever — so without a reverse lookup from B's basename back to
// whoever mentions it, that edge stays dangling for good.
func TestLaterArrivalRepairsDanglingLink(t *testing.T) {
	w, s, root, ctx := testWatcher(t)

	src := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/source.md", `---
id: `+src+`
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Refers to [[late-target]] which does not exist yet.
`)
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	srcID := uuid.MustParse(src)
	if dsts, err := w.store().LinkDsts(ctx, srcID); err != nil {
		t.Fatal(err)
	} else if len(dsts) != 0 {
		t.Fatalf("precondition: link should be dangling before the target exists, got %v", dsts)
	}

	// The target arrives in a separate run. source.md is untouched.
	target := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/late-target.md", entryContent(target))
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}

	dsts, err := w.store().LinkDsts(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsts) != 1 || dsts[0] != uuid.MustParse(target) {
		t.Fatalf("edge to a later-arriving target was never resolved: got %v", dsts)
	}
}

// The same repair must happen on a sanctioned write, not only on reconcile —
// write_note is how most notes actually land.
func TestLaterArrivalRepairsOnPipelineWrite(t *testing.T) {
	w, s, root, ctx := testWatcher(t)

	src := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/source.md", `---
id: `+src+`
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Refers to [[written-target]] which does not exist yet.
`)
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}

	target := uuid.Must(uuid.NewV7()).String()
	pipe := write.NewPipeline(w.indexer(), root, vault.NewHistory(root), nil)
	if err := pipe.Write(ctx, "entries/written-target.md", entryContent(target), ""); err != nil {
		t.Fatal(err)
	}

	dsts, err := w.store().LinkDsts(ctx, uuid.MustParse(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(dsts) != 1 || dsts[0] != uuid.MustParse(target) {
		t.Fatalf("write_note did not repair the dangling edge pointing at it: got %v", dsts)
	}
}

// TestAtomicSaveKeepsInboundLinks — vim, VS Code and most editors save by
// writing a temp file and renaming it over the target. fsnotify reports that
// as Rename/Remove on the target path, and treating it as a deletion drops the
// note row, which cascades every *inbound* link away. The follow-up Create
// re-indexes the note and its outbound links, but nothing restores its
// referrers: those notes did not change, so reconcile skips them by content
// hash forever.
//
// Observed in the real vault — editing one note's frontmatter took the graph
// from 7 edges to 6, and the lost edge was the one pointing *at* it. Ordinary
// editing silently degraded the graph leg of retrieval.
func TestAtomicSaveKeepsInboundLinks(t *testing.T) {
	w, s, root, ctx := testWatcher(t)

	target := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/target.md", entryContent(target))
	src := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/source.md", `---
id: `+src+`
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Refers to [[target]] in the body.
`)
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	srcID := uuid.MustParse(src)
	if dsts, err := s.LinkDsts(ctx, srcID); err != nil {
		t.Fatal(err)
	} else if len(dsts) != 1 {
		t.Fatalf("precondition: source should link to target before the edit, got %v", dsts)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()
	time.Sleep(200 * time.Millisecond)

	// The atomic save. The temp file is not .md, so it draws no events of its
	// own — only the rename onto the watched path does.
	final := filepath.Join(root, "entries", "target.md")
	tmp := filepath.Join(root, "entries", "target.md.swaptmp")
	if err := os.WriteFile(tmp, []byte(entryContent(target)+"\nEdited in place.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, final); err != nil {
		t.Fatal(err)
	}
	// Settle window (200ms) plus index, referrer repair and the history commit.
	time.Sleep(2 * time.Second)

	if _, err := s.GetNote(ctx, "entries/target.md"); err != nil {
		t.Fatalf("target note missing after an atomic save: %v", err)
	}
	dsts, err := s.LinkDsts(ctx, srcID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsts) != 1 || dsts[0] != uuid.MustParse(target) {
		t.Fatalf("inbound edge lost to an atomic save: got %v, want the edge to %s", dsts, target)
	}
}

// The settle window must not swallow a real deletion — that is the behaviour it
// is inserted in front of, and the one users rely on when they delete a note.
func TestRealDeletionStillRemovesTheNote(t *testing.T) {
	w, s, root, ctx := testWatcher(t)
	id := uuid.Must(uuid.NewV7()).String()
	writeEntry(t, root, "entries/doomed.md", entryContent(id))
	if err := w.Reconcile(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, "entries/doomed.md"); err != nil {
		t.Fatalf("precondition: note should be indexed, got %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()
	time.Sleep(200 * time.Millisecond)

	if err := os.Remove(filepath.Join(root, "entries", "doomed.md")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // settle window elapses, then the delete lands

	if _, err := s.GetNote(ctx, "entries/doomed.md"); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("deleted note still indexed after the settle window: %v", err)
	}
}
