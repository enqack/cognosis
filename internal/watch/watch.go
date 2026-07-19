package watch

import (
	"context"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/zeebo/blake3"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/vault"
)

// Run is the live half of out-of-band edit detection: an fsnotify loop over the vault plus the
// periodic sweep (default 60m, config-driven) that closes what events can't
// see -- suppressed-window edits and mtime-preserving editors. Respects ctx
// for shutdown; implements daemon.Runner.
func (w *Watcher) Run(ctx context.Context) error {
	const op = "watch.Run"
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	defer func() { _ = fw.Close() }()

	root := w.cfg.KBPath
	if err := addRecursive(fw, root); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}

	sweep := time.NewTicker(w.cfg.ReconcileSweepInterval)
	defer sweep.Stop()
	w.log.Info("watching vault", "root", root, "sweep_interval", w.cfg.ReconcileSweepInterval)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sweep.C:
			if err := w.reconcile(ctx, true); err != nil && ctx.Err() == nil {
				w.log.Error("periodic sweep failed", "reason", err)
			}
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ctx, fw, ev)
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.log.Error("watcher error", "reason", err)
		}
	}
}

// shutdownGrace bounds how long one in-flight unit of watcher work -- a single
// file's index (including its embedding call) plus the link repair and history
// commit that belong to it -- may keep running after shutdown cancellation.
// Units are shielded with context.WithoutCancel because aborting one mid-write
// is exactly what the daemon's runner drain exists to avoid: the drain waited
// for the watcher, but cancellation propagated *into* the write and killed the
// embed call the drain was waiting on, so the edit was dropped until the next
// boot reconciliation. Must stay below the daemon's runnerDrainTimeout (15s)
// so the watcher exits before the drain gives up and releases the locks.
const shutdownGrace = 10 * time.Second

// graceful returns the context for one atomic unit of watcher work: it
// ignores cancellation of ctx -- an already-started unit runs to completion --
// but is bounded, so a hung embed call cannot wedge shutdown past the drain.
// Cancellation is honoured *between* units by every caller.
func graceful(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
}

func (w *Watcher) handleEvent(ctx context.Context, fw *fsnotify.Watcher, ev fsnotify.Event) {
	// Shutdown already requested: start no new unit. The Run select can pick
	// a pending event over ctx.Done -- selection among ready cases is random.
	if ctx.Err() != nil {
		return
	}
	// New directories need watching (fsnotify is not recursive).
	if ev.Op.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if filepath.Base(ev.Name) != ".git" {
				if err := addRecursive(fw, ev.Name); err != nil {
					w.log.Error("watch new directory failed", "path", ev.Name, "reason", err)
				}
			}
			return
		}
	}

	if !strings.HasSuffix(ev.Name, ".md") {
		return
	}
	rel, err := filepath.Rel(w.cfg.KBPath, ev.Name)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, ".git/") || vault.IsReserved(rel) {
		return
	}
	if _, ok := vault.StageOf(rel); !ok {
		return
	}
	if _, suppressed := w.suppressed.Load(rel); suppressed {
		return // Cognosis's own write in progress; the periodic sweep bounds the risk
	}

	// One event is one atomic unit: index (or delete), repair, commit. Once
	// started it finishes even if shutdown cancels ctx mid-embed.
	gctx, done := graceful(ctx)
	defer done()

	switch {
	case ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename):
		// An atomic save -- write a temp file, rename it over the target -- reports
		// Rename (or Remove) on the target path before the Create that replaces
		// it. Treating that as a deletion drops the note row, and links cascades
		// every *inbound* edge away; the follow-up index restores the note and its
		// outbound links but not its referrers, because those notes did not change
		// and reconcile skips unchanged files by content hash. vim, VS Code and
		// most editors save this way, so an ordinary edit silently cost a note its
		// inbound graph until the next full rebuild.
		//
		// Let the path settle first. If it is back, this was a replacement and the
		// Create arm indexes it; a genuine deletion stays gone.
		if w.pathSettles(ev.Name) {
			return
		}
		if err := w.store().DeleteNote(gctx, rel); err != nil {
			w.log.Error("delete on event failed", "path", rel, "reason", err)
			return
		}
		w.log.Info("note removed (watch)", "path", rel)
		w.commitDrift(gctx, "watch: "+rel+" removed out-of-band")
	case ev.Op.Has(fsnotify.Create) || ev.Op.Has(fsnotify.Write):
		info, err := os.Stat(ev.Name)
		if err != nil {
			return // vanished between event and stat; a later event will cover it
		}
		content, err := os.ReadFile(ev.Name)
		if err != nil {
			w.log.Error("read on event failed", "path", rel, "reason", err)
			return
		}
		sum := blake3.Sum256(content)
		w.HashCount.Add(1)
		if err := w.indexFile(gctx, rel, content, info.ModTime(), info.Size(), hex.EncodeToString(sum[:])); err != nil {
			return // logged inside; invalid edits stay unindexed
		}
		// Belt to pathSettles' braces. A note can still reach this arm having been
		// absent from the index a moment earlier -- a real delete-then-restore, a
		// git checkout, or an event race the settle window loses -- and anything
		// pointing at it lost its edge to the cascade. Repairing referrers is one
		// basename-keyed query and idempotent, so it costs little to always run.
		w.repairReferrersOf(gctx, rel)
		w.commitDrift(gctx, "watch: "+rel+" edited out-of-band")
	}
}

// renameSettle bounds how long a Remove/Rename event waits for the path to
// come back before it is believed. It only has to outlast the gap between
// rename(2) landing and the event reaching us, which is microseconds of real
// work -- the window is generous because the cost of being wrong is asymmetric:
// waiting 200ms delays a deletion nobody is watching for, while not waiting
// silently drops the note's inbound edges on every editor save.
const (
	renameSettle     = 200 * time.Millisecond
	renameSettleStep = 20 * time.Millisecond
)

// pathSettles reports whether path reappears within renameSettle -- i.e. the
// Remove/Rename event was an atomic replacement rather than a deletion. It
// polls rather than waiting the full window so the common case (an editor
// save, where the file is already back) returns almost immediately.
//
// Blocking the event loop is deliberate: the loop is serial and already does
// far slower synchronous work per event (index, embed, git commit), and
// handling the replacement out of order would reintroduce the delete.
func (w *Watcher) pathSettles(path string) bool {
	for waited := time.Duration(0); waited < renameSettle; waited += renameSettleStep {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(renameSettleStep)
	}
	_, err := os.Stat(path)
	return err == nil
}

func (w *Watcher) commitDrift(ctx context.Context, msg string) {
	if err := w.hist.CommitAll(ctx, msg); err != nil {
		w.log.Error("history commit failed", "reason", err)
	}
}

func addRecursive(fw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		return fw.Add(path)
	})
}
