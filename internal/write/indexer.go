// Package write owns the sanctioned write path (write_note/write_reflection)
// and the shared note-indexing core that both the pipeline and the watcher's
// reconciliation use — one implementation, so a hand-edit and an MCP write
// index identically.
package write

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/chunk"
	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
)

// EmbedTarget is one (provider, table) pair a write embeds into.
type EmbedTarget struct {
	Provider embed.Provider
	Table    string
}

// Indexer turns a parsed, validated note into its complete derived state
// (note row, chunks, embeddings, links) in one store transaction.
type Indexer struct {
	Store    *store.Store
	Provider embed.Provider // nil = index without embeddings
	Table    string         // active provider table; "" with nil Provider
	// TargetsFn, when set, overrides Provider/Table with the current embed
	// targets per write — during a provider migration it returns the active
	// provider plus the in-progress one, so new writes are born fully covered
	// in both tables and never depend on the migration paths.
	TargetsFn func(ctx context.Context) ([]EmbedTarget, error)
}

// targets resolves where this write embeds.
func (ix *Indexer) targets(ctx context.Context) ([]EmbedTarget, error) {
	if ix.TargetsFn != nil {
		return ix.TargetsFn(ctx)
	}
	if ix.Provider == nil {
		return nil, nil
	}
	return []EmbedTarget{{Provider: ix.Provider, Table: ix.Table}}, nil
}

// Index validates nothing — callers pass a note that already satisfies the
// contract. FileMeta carries the on-disk identity for reconciliation.
type FileMeta struct {
	Mtime  time.Time
	Size   int64
	Blake3 string
}

func (ix *Indexer) Index(ctx context.Context, n *vault.Note, meta FileMeta) error {
	const op = "write.Index"

	sn, err := toStoreNote(n, meta)
	if err != nil {
		return err
	}

	chunks := chunk.Split(n)
	storeChunks := make([]store.Chunk, len(chunks))
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		storeChunks[i] = store.Chunk{
			Ordinal:     c.Ordinal,
			HeadingPath: c.HeadingPath,
			Content:     c.Content,
			ContentHash: c.Hash,
		}
		texts[i] = c.Content
	}

	targets, err := ix.targets(ctx)
	if err != nil {
		return err
	}
	vecsByTable := map[string]map[int][]float32{}
	if len(texts) > 0 {
		for _, tgt := range targets {
			embedded, err := tgt.Provider.Embed(ctx, texts)
			if err != nil {
				return cogerr.E(op, cogerr.Unavailable, err)
			}
			vecs := make(map[int][]float32, len(embedded))
			for i, v := range embedded {
				vecs[chunks[i].Ordinal] = v
			}
			vecsByTable[tgt.Table] = vecs
		}
	}

	links, err := ix.resolveLinks(ctx, n)
	if err != nil {
		return err
	}
	return ix.Store.IndexNote(ctx, sn, storeChunks, vecsByTable, links)
}

// resolveLinks maps the note's outbound wikilink/source targets to note ids;
// dangling targets are dropped (they become resolvable when their note lands).
func (ix *Indexer) resolveLinks(ctx context.Context, n *vault.Note) ([]store.Link, error) {
	refs := vault.Targets(n)
	if len(refs) == 0 {
		return nil, nil
	}
	key := func(r vault.Ref) string {
		if r.Project != "" {
			return r.Project + ":" + r.Name
		}
		return r.Name
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, key(r))
	}
	resolved, err := ix.Store.ResolveBasenames(ctx, names)
	if err != nil {
		return nil, err
	}
	selfID, _ := uuid.Parse(n.ID())
	var out []store.Link
	for _, r := range refs {
		dst, ok := resolved[key(r)]
		if !ok || dst == selfID {
			continue // dangling or self-link
		}
		out = append(out, store.Link{Dst: dst, Kind: string(r.Kind)})
	}
	return out, nil
}

// toStoreNote converts a contract-valid vault note plus file identity into
// the store row shape.
func toStoreNote(n *vault.Note, meta FileMeta) (store.Note, error) {
	const op = "write.toStoreNote"
	id, err := uuid.Parse(n.ID())
	if err != nil {
		return store.Note{}, cogerr.Ef(op, cogerr.Validation, "bad id in %s: %v", n.Path, err)
	}
	created, err := vault.TimeOf(n.Frontmatter["created"])
	if err != nil {
		return store.Note{}, cogerr.Ef(op, cogerr.Validation, "bad created in %s: %v", n.Path, err)
	}
	updated, err := vault.TimeOf(n.Frontmatter["updated"])
	if err != nil {
		return store.Note{}, cogerr.Ef(op, cogerr.Validation, "bad updated in %s: %v", n.Path, err)
	}
	sn := store.Note{
		Path:        n.Path,
		ID:          id,
		Project:     n.Project(),
		Category:    n.Category(),
		Status:      n.Status(),
		Created:     created.UTC(),
		Updated:     updated.UTC(),
		Frontmatter: n.Frontmatter,
		Content:     n.Body,
		Mtime:       meta.Mtime.UTC().Truncate(time.Microsecond),
		Size:        meta.Size,
		Blake3:      meta.Blake3,
	}
	if c, ok := n.Frontmatter["confidence"]; ok {
		if f, ok := toFloat(c); ok {
			sn.Confidence = &f
		}
	}
	if m, ok := n.Frontmatter["maturity"].(string); ok && m != "" {
		sn.Maturity = &m
	}
	if sum, ok := n.Frontmatter["summary"].(string); ok {
		sn.Summary = strings.TrimSpace(sum)
	}
	return sn, nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}
