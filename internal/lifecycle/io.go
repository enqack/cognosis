package lifecycle

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zeebo/blake3"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

// rewrite serializes the mutated note, re-parses and validates the result
// (the mutation must not have produced an invalid file), writes it atomically
// with the watcher suppressed, and reindexes through the shared core.
func (e *Engine) rewrite(ctx context.Context, n *vault.Note, rel string) error {
	const op = "lifecycle.rewrite"
	out, err := n.Serialize()
	if err != nil {
		return err
	}
	reparsed, err := vault.ParseNote(rel, out)
	if err != nil {
		return err
	}
	if probs := vault.Validate(rel, reparsed.Frontmatter, reparsed.Frontmatter != nil); len(probs) > 0 {
		msgs := make([]string, len(probs))
		for i, p := range probs {
			msgs[i] = p.String()
		}
		return cogerr.Ef(op, cogerr.Internal,
			"refusing to write invalid frontmatter (lifecycle bug): %s", strings.Join(msgs, "; "))
	}

	if e.Supp != nil {
		e.Supp.Suppress(rel)
		defer e.Supp.Unsuppress(rel)
	}
	abs := filepath.Join(e.VaultDir, filepath.FromSlash(rel))
	if err := vault.WriteFileAtomic(abs, out); err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	sum := blake3.Sum256(out)
	return e.Indexer.Index(ctx, reparsed, write.FileMeta{
		Mtime: info.ModTime(), Size: info.Size(), Blake3: hex.EncodeToString(sum[:]),
	})
}

// move rewrites the (already mutated) note at a new stage path and removes
// the old file. The note id is stable, so the index treats it as a move —
// inbound links survive.
func (e *Engine) move(ctx context.Context, n *vault.Note, dest string) error {
	const op = "lifecycle.move"
	src := n.Path
	absDest := filepath.Join(e.VaultDir, filepath.FromSlash(dest))
	if _, err := os.Stat(absDest); err == nil {
		return cogerr.Ef(op, cogerr.Conflict, "move %s: destination %s already exists", src, dest)
	}
	stage, ok := vault.StageOf(dest)
	if !ok {
		return cogerr.Ef(op, cogerr.Internal, "move destination %q is not a valid vault stage path", dest)
	}
	n.Path = dest
	n.Stage = stage
	if err := e.rewrite(ctx, n, dest); err != nil {
		return err
	}
	if e.Supp != nil {
		e.Supp.Suppress(src)
		defer e.Supp.Unsuppress(src)
	}
	if err := os.Remove(filepath.Join(e.VaultDir, filepath.FromSlash(src))); err != nil && !os.IsNotExist(err) {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// appendLog appends the run report to the vault's append-only log.md.
func (e *Engine) appendLog(r *Report) error {
	const op = "lifecycle.appendLog"
	f, err := os.OpenFile(filepath.Join(e.VaultDir, "log.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("\n" + r.String()); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := f.Close(); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// --- small helpers ---

func toSet(xs []string) map[string]bool {
	s := map[string]bool{}
	for _, x := range xs {
		if x != "" {
			s[x] = true
		}
	}
	return s
}

func toKeys(m map[string]string) map[string]bool {
	s := map[string]bool{}
	for k := range m {
		s[k] = true
	}
	return s
}

func pick(m map[string]string, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return "", false
}

func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

func wikiname(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".md")
}

// roundDays renders a duration in whole days for report lines — "43d" reads
// better than "1032h0m0s" in a log an agent has to skim.
func roundDays(d time.Duration) string {
	days := int(d.Round(24*time.Hour) / (24 * time.Hour))
	if days < 1 {
		return "under a day"
	}
	return strconv.Itoa(days) + "d"
}

// round1 snaps a confidence value to one decimal place.
func round1(f float64) float64 {
	return math.Round(f*10) / 10
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		// SetFM writes scalars as strings; a re-run within one process may see
		// the mutated map.
		var f float64
		if _, err := fmt.Sscanf(x, "%f", &f); err == nil {
			return f
		}
	}
	return 0
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}
