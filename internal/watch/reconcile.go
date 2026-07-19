// Package watch owns out-of-band edit detection: boot-time reconciliation
// (mtime/size fast path + BLAKE3 drift confirmation), the live fsnotify
// watcher, and the periodic sweep that closes the drift windows the other
// two can't see. Confirmed drift is committed to the vault history repo as
// found.
package watch

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/blake3"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/daemon"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

// Watcher implements daemon.Reconciler and daemon.Runner.
type Watcher struct {
	cfg  *config.Config
	log  *slog.Logger
	hist *vault.History

	// MakeIndexer builds the indexing core once the store is connected; the
	// daemon wires in the embedding provider here so hand-edits index (and
	// embed) identically to sanctioned writes. nil falls back to a bare
	// indexer with no embeddings.
	MakeIndexer func(s *store.Store) *write.Indexer

	mu sync.Mutex
	st *store.Store
	ix *write.Indexer

	// suppressed paths are being written by Cognosis itself right now; their
	// disk events are dropped (write-conflict handling). The periodic
	// sweep is what bounds the drift this can cause.
	suppressed sync.Map

	// HashCount counts BLAKE3 hashes computed -- tests assert the fast path
	// via this counter, never via timing.
	HashCount atomic.Int64
}

func New(cfg *config.Config, log *slog.Logger) *Watcher {
	return &Watcher{
		cfg:  cfg,
		log:  log.With("component", "watch"),
		hist: vault.NewHistory(cfg.KBPath),
	}
}

// Suppress marks a vault-relative path as being written by Cognosis; the
// watcher ignores its events until Unsuppress. (The write pipeline is the
// caller.)
func (w *Watcher) Suppress(rel string)   { w.suppressed.Store(rel, true) }
func (w *Watcher) Unsuppress(rel string) { w.suppressed.Delete(rel) }

// Reconcile is the boot-time integrity check: fast path compares
// mtime+size against the stored state; only differing files get hashed
// (worker pool) and, on confirmed drift, re-validated and re-indexed.
func (w *Watcher) Reconcile(ctx context.Context, s *store.Store) error {
	w.mu.Lock()
	w.st = s
	if w.MakeIndexer != nil {
		w.ix = w.MakeIndexer(s)
	} else {
		w.ix = &write.Indexer{Store: s}
	}
	w.mu.Unlock()
	return w.reconcile(ctx, false)
}

// reconcile walks the vault once. forceHash bypasses the mtime/size fast path
// -- the periodic sweep uses it to catch editors that preserve both.
func (w *Watcher) reconcile(ctx context.Context, forceHash bool) error {
	s := w.store()
	states, err := s.FileStates(ctx)
	if err != nil {
		return err
	}

	var candidates []candidate
	onDisk := map[string]bool{}

	root := w.cfg.KBPath
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if vault.IsReserved(rel) {
			return nil
		}
		if _, ok := vault.StageOf(rel); !ok {
			return nil
		}
		onDisk[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !forceHash {
			if st, known := states[rel]; known && st.Size == info.Size() && st.Mtime.Equal(info.ModTime().UTC().Truncate(time.Microsecond)) {
				return nil // fast path: unchanged on both, skip (no hash)
			}
		}
		candidates = append(candidates, candidate{rel, path, info.ModTime(), info.Size()})
		return nil
	})
	if err != nil {
		return err
	}

	// Hash candidates in a worker pool; only confirmed drift gets re-indexed.
	var (
		driftMu sync.Mutex
		drifts  []drifted
	)
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			defer daemon.RecoverPanic(w.log, "watch.reconcile hash worker", func(error) {
				w.log.Error("reconcile hash worker recovered from panic", "path", c.rel)
			})
			content, err := os.ReadFile(c.abs)
			if err != nil {
				w.log.Error("reconcile read failed", "path", c.rel, "reason", err)
				return
			}
			sum := blake3.Sum256(content)
			w.HashCount.Add(1)
			hash := hex.EncodeToString(sum[:])
			if st, known := states[c.rel]; known && st.Blake3 == hash {
				return // mtime/size differed but content didn't -- refresh nothing
			}
			driftMu.Lock()
			drifts = append(drifts, drifted{c, hash, content})
			driftMu.Unlock()
		}(c)
	}
	wg.Wait()

	// The hash workers append concurrently, so drifts arrives in completion
	// order -- meaning the index order, and therefore which links resolve on
	// the first pass, varies run to run. Sort so a reconcile is reproducible
	// and so tests can pin the order that used to lose edges.
	sort.Slice(drifts, func(i, j int) bool { return drifts[i].rel < drifts[j].rel })

	changed := 0
	indexed := make([]drifted, 0, len(drifts))
	for _, d := range drifts {
		// Cancellation is honoured between files, never inside one: a started
		// index (embed call included) runs to completion under graceful so a
		// shutdown mid-write cannot drop the edit it interrupted.
		if ctx.Err() != nil {
			break
		}
		gctx, done := graceful(ctx)
		err := w.indexFile(gctx, d.rel, d.content, d.mtime, d.size, d.hash)
		done()
		if err == nil {
			changed++
			indexed = append(indexed, d)
		}
	}

	// Second pass: re-resolve links now that every note from this batch is in
	// the index. Link resolution matches against notes already indexed, so on
	// the first pass a note whose target came later in the loop silently lost
	// that edge -- and nothing repaired it afterwards, because reconciliation
	// confirms drift by content hash and an unchanged file is skipped forever.
	// A rebuild after dropping the schema therefore produced a partial graph.
	// Cost is one resolve + SetLinks per note; no chunking, no embedding.
	//
	// This closes the within-batch ordering hole, which is the one a rebuild
	// hits. The later-arrival case -- A references B, B created in some later
	// run, A unchanged and so never revisited -- is closed by the third pass.
	if len(indexed) > 1 {
		w.relinkBatch(ctx, indexed)
	}
	// Third pass: repair notes *outside* this batch that reference something
	// in it. Those referrers are unchanged on disk, so drift detection will
	// never look at them again on its own, and their edge to a note that only
	// just landed would stay dangling forever.
	if len(indexed) > 0 {
		w.repairReferrers(ctx, indexed)
	}

	// Deletions: indexed paths that no longer exist on disk.
	for rel := range states {
		if ctx.Err() != nil {
			break // shutdown: whatever was indexed above still gets committed
		}
		if !onDisk[rel] {
			if err := s.DeleteNote(ctx, rel); err != nil {
				w.log.Error("reconcile delete failed", "path", rel, "reason", err)
				continue
			}
			w.log.Info("note removed (file gone)", "path", rel)
			changed++
		}
	}

	if changed > 0 {
		// Commit the drift as found -- hand-edits get history too. Shielded:
		// files already indexed must reach vault history even when this run is
		// ending on a shutdown, or the tree is left dirty with the index ahead
		// of history -- the exact state the daemon's runner drain exists to
		// prevent.
		gctx, done := graceful(ctx)
		defer done()
		if err := w.hist.CommitAll(gctx, fmt.Sprintf("reconcile: %d file(s) drifted out-of-band", changed)); err != nil {
			w.log.Error("history commit failed", "reason", err)
		}
		w.log.Info("reconciliation applied", "changed", changed, "hashed", len(candidates))
	}
	return nil
}

// candidate is a vault file the fast path could not rule out.
type candidate struct {
	rel   string
	abs   string
	mtime time.Time
	size  int64
}

// drifted is one file whose on-disk content differs from the index.
type drifted struct {
	candidate
	hash    string
	content []byte
}

// relinkBatch re-resolves outbound links for every note indexed in this run,
// repairing edges that were dangling when their note was indexed ahead of its
// targets. Failures are logged, not fatal: a missing edge degrades the graph
// leg of retrieval, it does not corrupt the index.
func (w *Watcher) relinkBatch(ctx context.Context, batch []drifted) {
	repaired := 0
	for _, d := range batch {
		if ctx.Err() != nil {
			return
		}
		n, err := vault.ParseNote(d.rel, d.content)
		if err != nil {
			continue // already reported by indexFile
		}
		if err := w.indexer().Relink(ctx, n); err != nil {
			w.log.Error("relink failed", "path", d.rel, "reason", err)
			continue
		}
		repaired++
	}
	w.log.Debug("links re-resolved after batch index", "notes", repaired)
}

// repairReferrers re-resolves links of notes that reference anything just
// indexed, excluding the batch itself (relinkBatch already covered it).
func (w *Watcher) repairReferrers(ctx context.Context, batch []drifted) {
	names := make([]string, 0, len(batch))
	skip := make(map[string]bool, len(batch))
	for _, d := range batch {
		names = append(names, strings.TrimSuffix(path.Base(d.rel), ".md"))
		skip[d.rel] = true
	}
	n, err := w.indexer().RepairReferrers(ctx, names, skip)
	if err != nil {
		w.log.Error("referrer link repair failed", "reason", err)
		return
	}
	if n > 0 {
		w.log.Info("repaired links in notes referencing newly indexed files", "notes", n)
	}
}

// repairReferrersOf is repairReferrers for a single path -- the watch-event
// path, where notes arrive one at a time rather than in a reconcile batch.
func (w *Watcher) repairReferrersOf(ctx context.Context, rel string) {
	name := strings.TrimSuffix(path.Base(rel), ".md")
	n, err := w.indexer().RepairReferrers(ctx, []string{name}, map[string]bool{rel: true})
	if err != nil {
		// Not fatal, matching repairReferrers: a missing edge degrades the graph
		// leg, it does not corrupt the index, and the note itself is already in.
		w.log.Error("referrer link repair failed", "path", rel, "reason", err)
		return
	}
	if n > 0 {
		w.log.Info("repaired links in notes referencing an out-of-band edit", "path", rel, "notes", n)
	}
}

// indexFile validates one file and routes it through the shared indexing
// core (chunks, embeddings, links -- identical to a sanctioned write). Invalid
// frontmatter is logged as a sync error and NOT indexed -- the previous DB
// state survives.
func (w *Watcher) indexFile(ctx context.Context, rel string, content []byte, mtime time.Time, size int64, hash string) error {
	n, err := vault.ParseNote(rel, content)
	if err != nil {
		w.log.Error("sync error: unparseable note", "path", rel, "reason", err)
		return err
	}
	if probs := vault.Validate(rel, n.Frontmatter, n.Frontmatter != nil); len(probs) > 0 {
		for _, p := range probs {
			w.log.Error("sync error: contract violation", "path", rel, "field", p.Field, "reason", p.Reason)
		}
		return fmt.Errorf("contract violation in %s", rel)
	}
	meta := write.FileMeta{Mtime: mtime, Size: size, Blake3: hash}
	if err := w.indexer().Index(ctx, n, meta); err != nil {
		w.log.Error("sync error: index failed", "path", rel, "reason", err)
		return err
	}
	w.log.Info("note indexed", "path", rel)
	return nil
}

func (w *Watcher) store() *store.Store {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st
}

func (w *Watcher) indexer() *write.Indexer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ix
}
