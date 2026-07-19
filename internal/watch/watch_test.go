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
	writeEntry(t, root, "entries/mine.md", entryContent(uuid.NewString()))
	time.Sleep(500 * time.Millisecond) // would have been indexed by now if not suppressed
	if _, err := s.GetNote(ctx, "entries/mine.md"); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("suppressed path was indexed anyway: %v", err)
	}
	w.Unsuppress("entries/mine.md")
}
