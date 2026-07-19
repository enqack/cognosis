package write

import (
	"context"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/vault"
)

// Suppressor mutes watcher events for a path while Cognosis itself writes it;
// the watch package implements it.
type Suppressor interface {
	Suppress(rel string)
	Unsuppress(rel string)
}

type noopSuppressor struct{}

func (noopSuppressor) Suppress(string)   {}
func (noopSuppressor) Unsuppress(string) {}

// Pipeline is the sanctioned write path: validate → per-path lock → atomic
// file write (watcher suppressed) → one history commit → chunk → embed →
// single-transaction index.
type Pipeline struct {
	Indexer  *Indexer
	VaultDir string
	Hist     *vault.History
	Supp     Suppressor

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewPipeline(ix *Indexer, vaultDir string, hist *vault.History, supp Suppressor) *Pipeline {
	if supp == nil {
		supp = noopSuppressor{}
	}
	return &Pipeline{
		Indexer:  ix,
		VaultDir: vaultDir,
		Hist:     hist,
		Supp:     supp,
		locks:    map[string]*sync.Mutex{},
	}
}

// pathLock returns the mutex for one vault-relative path, creating it on
// first use. Concurrent writes to the same path serialize; different paths
// proceed independently.
func (p *Pipeline) pathLock(rel string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.locks[rel]
	if !ok {
		l = &sync.Mutex{}
		p.locks[rel] = l
	}
	return l
}

// Write validates and lands one note. project, when non-empty, must match the
// note's own frontmatter — the file is the source of truth, the argument is a
// cross-check.
func (p *Pipeline) Write(ctx context.Context, rel, content, project string) error {
	const op = "write.Pipeline.Write"

	rel = filepath.ToSlash(filepath.Clean(rel))
	if strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return cogerr.Ef(op, cogerr.Validation, "path %q escapes the vault", rel)
	}
	if !strings.HasSuffix(rel, ".md") {
		return cogerr.Ef(op, cogerr.Validation, "path %q: notes are markdown files", rel)
	}
	if vault.IsReserved(rel) {
		return cogerr.Ef(op, cogerr.Validation, "path %q is a reserved generated file", rel)
	}
	if _, ok := vault.StageOf(rel); !ok {
		return cogerr.Ef(op, cogerr.Validation,
			"path %q is outside the stage folders (entries/, notes/, reflections/, archive/)", rel)
	}

	n, err := vault.ParseNote(rel, []byte(content))
	if err != nil {
		return err
	}
	if probs := vault.Validate(rel, n.Frontmatter, n.Frontmatter != nil); len(probs) > 0 {
		msgs := make([]string, len(probs))
		for i, pr := range probs {
			msgs[i] = pr.String()
		}
		return cogerr.Ef(op, cogerr.Validation, "%s", strings.Join(msgs, "; "))
	}
	if project != "" && n.Project() != project {
		return cogerr.Ef(op, cogerr.Validation,
			"project argument %q does not match the note's frontmatter project %q", project, n.Project())
	}

	lock := p.pathLock(rel)
	lock.Lock()
	defer lock.Unlock()

	p.Supp.Suppress(rel)
	defer p.Supp.Unsuppress(rel)

	abs := filepath.Join(p.VaultDir, filepath.FromSlash(rel))
	if err := vault.WriteFileAtomic(abs, []byte(content)); err != nil {
		return err
	}
	if p.Hist != nil {
		if err := p.Hist.CommitAll(ctx, "write_note: "+rel); err != nil {
			return err
		}
	}

	info, err := os.Stat(abs)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	sum := blake3.Sum256([]byte(content))
	meta := FileMeta{Mtime: info.ModTime(), Size: info.Size(), Blake3: hex.EncodeToString(sum[:])}

	if err := p.Indexer.Index(ctx, n, meta); err != nil {
		return err
	}

	// A note landing can resolve edges that were dangling: anything already in
	// the vault referencing this basename dropped that link when it was
	// indexed, and would never be revisited on its own (it is unchanged, so
	// drift detection skips it). Repair those now, while the fact that this
	// note just arrived is known.
	//
	// Non-fatal: the write itself succeeded, and a missing edge degrades the
	// graph leg of retrieval rather than corrupting anything.
	base := strings.TrimSuffix(path.Base(rel), ".md")
	if _, err := p.Indexer.RepairReferrers(ctx, []string{base}, map[string]bool{rel: true}); err != nil {
		return nil //nolint:nilerr // the write landed; link repair is best-effort
	}
	return nil
}
