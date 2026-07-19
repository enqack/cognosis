package store

import (
	"context"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
)

// IndexNote is the one-transaction write: upsert the note row, replace its
// chunks, upsert the chunks' embeddings into every given provider table (a
// write during a provider migration lands in both the active and the
// in-progress table), and replace its outbound links. Either everything lands
// or nothing does -- a crash between the file write and this commit is exactly
// what boot reconciliation repairs.
func (s *Store) IndexNote(ctx context.Context, n Note, chunks []Chunk,
	embedsByTable map[string]map[int][]float32, links []Link) error {
	const op = "store.IndexNote"
	for table := range embedsByTable {
		if !tableNameRe.MatchString(table) {
			return cogerr.Ef(op, cogerr.Validation, "bad embedding table name %q", table)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertNoteTx(ctx, tx, n); err != nil {
		return err
	}

	// Replace chunks, capturing generated ids by ordinal for the embeddings.
	if _, err := tx.Exec(ctx, `delete from chunks where note_path = $1`, n.Path); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	idByOrdinal := make(map[int]uuid.UUID, len(chunks))
	for _, c := range chunks {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			insert into chunks (note_path, ordinal, heading_path, content, content_hash)
			values ($1,$2,$3,$4,$5) returning id`,
			n.Path, c.Ordinal, nullIfEmpty(c.HeadingPath), c.Content, c.ContentHash).Scan(&id)
		if err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
		idByOrdinal[c.Ordinal] = id
	}

	for table, vecsByOrdinal := range embedsByTable {
		if len(vecsByOrdinal) == 0 {
			continue
		}
		byID := make(map[uuid.UUID][]float32, len(vecsByOrdinal))
		for ord, v := range vecsByOrdinal {
			id, ok := idByOrdinal[ord]
			if !ok {
				return cogerr.Ef(op, cogerr.Internal, "vector for unknown ordinal %d", ord)
			}
			byID[id] = v
		}
		if err := upsertEmbeddingsTx(ctx, tx, table, byID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `delete from links where src_note_id = $1`, n.ID); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	for _, l := range links {
		if _, err := tx.Exec(ctx, `
			insert into links (src_note_id, dst_note_id, kind) values ($1,$2,$3)
			on conflict do nothing`, n.ID, l.Dst, l.Kind); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// ResolveBasenames maps wikilink targets to note ids. Plain basenames (no
// directory, no .md) resolve first-match-wins across the whole KB; qualified
// "project:basename" targets resolve exactly within that project -- the
// cross-project disambiguation for colliding basenames. Names that resolve to
// no indexed note are simply absent from the result -- dangling links are
// dropped, not errors.
func (s *Store) ResolveBasenames(ctx context.Context, names []string) (map[string]uuid.UUID, error) {
	const op = "store.ResolveBasenames"
	out := make(map[string]uuid.UUID, len(names))
	if len(names) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `select path, project, id from notes order by path`)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()

	byBase := map[string]uuid.UUID{}
	byQualified := map[string]uuid.UUID{}
	for rows.Next() {
		var p, project string
		var id uuid.UUID
		if err := rows.Scan(&p, &project, &id); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		base := strings.TrimSuffix(path.Base(p), ".md")
		if _, seen := byBase[base]; !seen {
			byBase[base] = id
		}
		if project != "" {
			if q := project + ":" + base; byQualified[q] == uuid.Nil {
				byQualified[q] = id
			}
		}
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	for _, name := range names {
		if strings.Contains(name, ":") {
			if id, ok := byQualified[name]; ok {
				out[name] = id
			}
			continue
		}
		if id, ok := byBase[name]; ok {
			out[name] = id
		}
	}
	return out, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
