package write

import (
	"context"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"

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

	// Locks is shared with every other writer of the same vault — notably
	// lifecycle.Engine, which writes files directly. See PathLocks.
	Locks *PathLocks
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
		Locks:    NewPathLocks(),
	}
}

// rejectIDChange refuses a write that would give an existing path a different
// id.
//
// upsertNoteTx runs `delete from notes where path = $1 and id <> $2`, so a
// changed id evicts the row and cascades its inbound edges. ensureNoteID
// already avoids that for an *omitted* id, but that guard was one-sided: a
// supplied-but-different id evicted just the same, and edit_note — advertised
// for fixing a line or appending a source — made that a one-call operation on
// frontmatter the caller can now see and edit.
//
// The observable damage is subtler than "links are lost": RepairReferrers
// re-resolves wikilinks by basename after the write, so edges are re-pointed at
// the new id rather than dropped. What breaks is identity. An id is written
// once and never rewritten precisely so it survives moves and renames, and
// anything holding the old one — an external reference, an earlier retrieval
// result — now names a row that no longer exists.
//
// Conflict rather than Validation: the content is well-formed, it just
// disagrees with the note already at that path.
func (p *Pipeline) rejectIDChange(ctx context.Context, rel string, n *vault.Note) error {
	const op = "write.Pipeline.Write"
	if p.Indexer == nil || p.Indexer.Store == nil {
		return nil
	}
	supplied := strings.TrimSpace(n.ID())
	if supplied == "" {
		return nil // ensureNoteID owns this case
	}
	existing, err := p.Indexer.Store.GetNote(ctx, rel)
	if cogerr.Is(err, cogerr.NotFound) {
		return nil // new path: any valid id is fine
	}
	if err != nil {
		return err
	}
	if supplied == existing.ID.String() {
		return nil
	}
	return cogerr.Ef(op, cogerr.Conflict,
		"%s already has id %s; changing a note's id evicts it and re-points every inbound link. "+
			"Omit the id to keep the existing one, or write to a different path",
		rel, existing.ID)
}

// ensureNoteID fills in a missing frontmatter `id`, returning the content to
// write. Content that already carries one is returned untouched.
//
// The contract requires a UUIDv7 — ids sort lexically by creation, and an id is
// written once and never rewritten, so the version accepted at write time is
// permanent. But a caller holding only the MCP tools has no way to mint one:
// every note written during the session that surfaced this had to shell out to
// a Go program. Requiring a value the interface cannot produce is a defect in
// the interface, not a discipline.
//
// Reusing the existing id when the path is already indexed is the load-bearing
// half. Minting unconditionally would hand an existing path a *new* id, and
// UpsertNote treats same-path-different-id as an eviction: it deletes the row
// and cascades every inbound link away. Omitting the id on an update would
// then silently destroy that note's inbound graph — the same damage an atomic
// editor save used to do, arriving by a different route.
func (p *Pipeline) ensureNoteID(ctx context.Context, rel, content string) (string, error) {
	const op = "write.Pipeline.Write"

	// Parse failures and missing frontmatter are the caller's own validation
	// errors, reported with better context a few lines later. Say nothing here.
	n, err := vault.ParseNote(rel, []byte(content))
	if err != nil || n.Frontmatter == nil {
		// Deliberately swallowed: Write re-parses immediately after this and
		// reports the same failure with the field-level detail a caller can
		// act on. Returning it here would replace that with a bare parse error
		// from a function whose job is only to supply a missing id.
		return content, nil //nolint:nilerr // Write reports this with better context
	}
	if strings.TrimSpace(n.ID()) != "" {
		return content, nil
	}

	id := ""
	if p.Indexer != nil && p.Indexer.Store != nil {
		switch existing, err := p.Indexer.Store.GetNote(ctx, rel); {
		case err == nil:
			id = existing.ID.String()
		case cogerr.Is(err, cogerr.NotFound):
			// Genuinely new; fall through to minting.
		default:
			// Do not guess. Minting here would risk the eviction above on a
			// path that may well exist.
			return "", err
		}
	}
	if id == "" {
		if id, err = vault.NewNoteID(); err != nil {
			return "", cogerr.E(op, cogerr.Internal, err)
		}
	}

	// Splice textually rather than re-serializing the frontmatter: the vault is
	// the source of truth and the agent's bytes are written verbatim, so
	// round-tripping through a YAML marshaller would reorder keys and drop
	// comments on every write that happened to omit an id.
	const fence = "---\n"
	if !strings.HasPrefix(content, fence) {
		return content, nil // no opening fence; validation will say so
	}
	// A present-but-blank `id:` means the same thing as no id at all, and it is
	// one of the most natural ways to write "not filled in yet". Splicing on
	// top of it produced two `id` keys, and YAML rejects that with
	// `mapping key "id" already defined at line 1` — an error naming a line the
	// caller never wrote, on the exact case this feature exists to serve. Drop
	// the blank key rather than stacking a second one over it.
	body := dropBlankIDKey(content[len(fence):])
	return fence + "id: " + id + "\n" + body, nil
}

// dropBlankIDKey removes a top-level `id:` line with no value from the
// frontmatter block. Scoped to the frontmatter and to column zero so an `id:`
// inside a nested mapping, or inside the note body, is untouched.
func dropBlankIDKey(afterFence string) string {
	end := strings.Index(afterFence, "\n---")
	if end < 0 {
		return afterFence // unterminated frontmatter; validation reports it
	}
	fm, rest := afterFence[:end], afterFence[end:]
	lines := strings.Split(fm, "\n")
	out := lines[:0]
	for _, l := range lines {
		if after, ok := strings.CutPrefix(l, "id:"); ok && strings.TrimSpace(after) == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n") + rest
}

// Write validates and lands one note. project, when non-empty, must match the
// note's own frontmatter — the file is the source of truth, the argument is a
// cross-check.
func (p *Pipeline) Write(ctx context.Context, rel, content, project string) error {
	rel, err := checkPath(rel)
	if err != nil {
		return err
	}
	defer p.Locks.Lock(rel)()
	return p.writeLocked(ctx, rel, content, project)
}

// Edit replaces one exact, unique occurrence of oldStr with newStr in an
// existing note, then lands the result through the same path Write uses —
// validation, history, chunk, embed, index, link repair.
//
// It exists because write_note takes whole-file content, so changing one line
// of frontmatter meant resending several kilobytes verbatim. During the session
// that surfaced this, the pragmatic route for small changes was editing the
// file on disk and letting the watcher reconcile, which made the sanctioned
// write path the harder one — and cost a note its inbound links when an atomic
// save was misread as a deletion.
//
// Uniqueness is required rather than replacing the first match. A caller
// cannot see the file, so "first occurrence" is a guess about content they are
// not looking at; an ambiguous edit that silently picks one is the failure this
// design exists to avoid. Reporting the count lets the caller extend the
// snippet until it is unique.
//
// The read and the write happen under one path lock. Read-modify-write without
// it loses an edit whenever two land together, and the loser looks like it
// succeeded.
func (p *Pipeline) Edit(ctx context.Context, rel, oldStr, newStr string) error {
	const op = "write.Pipeline.Edit"

	rel, err := checkPath(rel)
	if err != nil {
		return err
	}
	if oldStr == "" {
		return cogerr.Ef(op, cogerr.Validation, "old_string is required; to create a note use write_note")
	}
	if oldStr == newStr {
		return cogerr.Ef(op, cogerr.Validation, "old_string and new_string are identical; nothing to do")
	}

	defer p.Locks.Lock(rel)()

	// The file, not the index: the vault is the source of truth, and the index
	// can legitimately lag it after an out-of-band edit.
	abs := filepath.Join(p.VaultDir, filepath.FromSlash(rel))
	current, err := os.ReadFile(abs) //nolint:gosec // path is vetted by checkPath
	if err != nil {
		if os.IsNotExist(err) {
			return cogerr.Ef(op, cogerr.NotFound, "no note at %s; use write_note to create it", rel)
		}
		return cogerr.E(op, cogerr.Internal, err)
	}

	switch n := strings.Count(string(current), oldStr); n {
	case 1:
		// The only case that can be applied unambiguously.
	case 0:
		return cogerr.Ef(op, cogerr.NotFound,
			"old_string does not appear in %s; it must match the file exactly, including whitespace", rel)
	default:
		return cogerr.Ef(op, cogerr.Validation,
			"old_string appears %d times in %s; extend it until it identifies one location", n, rel)
	}

	return p.writeLocked(ctx, rel, strings.Replace(string(current), oldStr, newStr, 1), "")
}

// checkPath normalises and vets a vault-relative path.
func checkPath(rel string) (string, error) {
	const op = "write.Pipeline.Write"
	rel = filepath.ToSlash(filepath.Clean(rel))
	if strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", cogerr.Ef(op, cogerr.Validation, "path %q escapes the vault", rel)
	}
	if !strings.HasSuffix(rel, ".md") {
		return "", cogerr.Ef(op, cogerr.Validation, "path %q: notes are markdown files", rel)
	}
	if vault.IsReserved(rel) {
		return "", cogerr.Ef(op, cogerr.Validation, "path %q is a reserved generated file", rel)
	}
	if _, ok := vault.StageOf(rel); !ok {
		return "", cogerr.Ef(op, cogerr.Validation,
			"path %q is outside the stage folders (entries/, notes/, reflections/, archive/)", rel)
	}
	return rel, nil
}

// writeLocked is Write's body, with the per-path lock already held. Edit needs
// to read the current file and write the result without another writer
// interleaving, so validation and the id lookup moved inside the lock: doing
// them outside left a window where the id check could observe one state and
// the write commit another.
func (p *Pipeline) writeLocked(ctx context.Context, rel, content, project string) error {
	const op = "write.Pipeline.Write"

	content, err := p.ensureNoteID(ctx, rel, content)
	if err != nil {
		return err
	}

	n, err := vault.ParseNote(rel, []byte(content))
	if err != nil {
		return err
	}
	if err := p.rejectIDChange(ctx, rel, n); err != nil {
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
