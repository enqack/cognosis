package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Note is a row in notes: full content mirror plus the promoted frontmatter
// columns and reconciliation state.
type Note struct {
	Path        string
	ID          uuid.UUID
	Project     string
	Category    string
	Status      string
	Confidence  *float64
	Maturity    *string
	Created     time.Time
	Updated     time.Time
	Frontmatter map[string]any
	Content     string
	Summary     string // agent-supplied one-liner, from the frontmatter summary key
	Mtime       time.Time
	Size        int64
	Blake3      string
}

// Chunk is a derived, droppable row; embeddings live in per-provider tables,
// never here.
type Chunk struct {
	Ordinal     int
	HeadingPath string
	Content     string
	ContentHash string
}

// Link is a resolved edge keyed on stable note ids.
type Link struct {
	Dst  uuid.UUID
	Kind string // "wikilink" | "source"
}

// UpsertNote writes a note row keyed on path. A note whose id already exists
// under a different path is a *move*: its path is updated in place (links are
// keyed on id and survive; chunks follow via on-update cascade) rather than
// delete+reinsert, which would cascade the graph away. A different note
// previously occupying the target path is evicted first.
func (s *Store) UpsertNote(ctx context.Context, n Note) error {
	const op = "store.UpsertNote"
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := upsertNoteTx(ctx, tx, n); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

func upsertNoteTx(ctx context.Context, tx pgxTx, n Note) error {
	const op = "store.upsertNote"
	fm, err := json.Marshal(n.Frontmatter)
	if err != nil {
		return cogerr.E(op, cogerr.Validation, err)
	}
	if _, err := tx.Exec(ctx,
		`delete from notes where path = $1 and id <> $2`, n.Path, n.ID); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if _, err := tx.Exec(ctx,
		`update notes set path = $2 where id = $1 and path <> $2`, n.ID, n.Path); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	_, err = tx.Exec(ctx, `
		insert into notes (path, id, project, category, status, confidence, maturity,
		                   created, updated, frontmatter, content, summary, mtime, size, blake3_hash, indexed_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15, now())
		on conflict (path) do update set
		  id = excluded.id, project = excluded.project, category = excluded.category,
		  status = excluded.status, confidence = excluded.confidence, maturity = excluded.maturity,
		  created = excluded.created, updated = excluded.updated, frontmatter = excluded.frontmatter,
		  content = excluded.content, summary = excluded.summary, mtime = excluded.mtime,
		  size = excluded.size, blake3_hash = excluded.blake3_hash, indexed_at = now()`,
		n.Path, n.ID, n.Project, n.Category, n.Status, n.Confidence, n.Maturity,
		n.Created, n.Updated, fm, n.Content, n.Summary, n.Mtime, n.Size, n.Blake3)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// GetNote fetches one note by path.
func (s *Store) GetNote(ctx context.Context, path string) (Note, error) {
	const op = "store.GetNote"
	var n Note
	var fm []byte
	err := s.pool.QueryRow(ctx, `
		select path, id, project, category, status, confidence, maturity,
		       created, updated, frontmatter, content, summary, mtime, size, blake3_hash
		from notes where path = $1`, path).Scan(
		&n.Path, &n.ID, &n.Project, &n.Category, &n.Status, &n.Confidence, &n.Maturity,
		&n.Created, &n.Updated, &fm, &n.Content, &n.Summary, &n.Mtime, &n.Size, &n.Blake3)
	if errors.Is(err, pgx.ErrNoRows) {
		return Note{}, cogerr.Ef(op, cogerr.NotFound, "note %q", path)
	}
	if err != nil {
		return Note{}, cogerr.E(op, cogerr.Internal, err)
	}
	if err := json.Unmarshal(fm, &n.Frontmatter); err != nil {
		return Note{}, cogerr.E(op, cogerr.Internal, err)
	}
	return n, nil
}

// NoteMeta is the content-free listing row for browse/introspection.
type NoteMeta struct {
	Path     string
	ID       uuid.UUID
	Project  string
	Category string
	Status   string
	Updated  time.Time
	Summary  string
}

// ListNotes returns metadata for every note, optionally project-scoped,
// newest-updated first. Never returns content — that's GetNote's job.
func (s *Store) ListNotes(ctx context.Context, project string) ([]NoteMeta, error) {
	const op = "store.ListNotes"
	rows, err := s.pool.Query(ctx, `
		select path, id, project, category, status, updated, summary from notes
		where ($1 = '' or project = $1)
		order by updated desc, path`, project)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var out []NoteMeta
	for rows.Next() {
		var m NoteMeta
		if err := rows.Scan(&m.Path, &m.ID, &m.Project, &m.Category, &m.Status, &m.Updated, &m.Summary); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, m)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// DecayingNote is one row of the staleness shortlist.
type DecayingNote struct {
	Path           string
	Project        string
	Confidence     float64
	Maturity       string
	LastReinforced string
	// LastAsserted is when an agent last *explicitly* reinforced the note,
	// falling back to created. This is what the shortlist is ordered by.
	LastAsserted string
}

// ListDecaying surfaces notes nobody has explicitly asserted since the cutoff
// — the shortlist to feed compile_lifecycle's reinforce. Decay-shielded notes
// (falsified, paused, graduated canon) are excluded. Comparison is
// lexicographic over the fixed-width timestamp layout, matching how the
// lifecycle reads frontmatter time.
//
// It keys on last_explicit_reinforce (falling back to created), NOT
// last_reinforced. last_reinforced is written by passive citation refresh and
// by decay, so a note being kept alive by citations — or actively decaying —
// looks freshly reinforced by that field and would drop off the very list that
// exists to surface it. The distinction only became load-bearing once decay
// started resetting the clock; before that these two orderings agreed often
// enough to hide the difference.
func (s *Store) ListDecaying(ctx context.Context, cutoff time.Time, project string) ([]DecayingNote, error) {
	const op = "store.ListDecaying"
	// asserted: the explicit anchor when present, else created — mirroring
	// lifecycle.passiveBudgetLeft so the two never disagree about a note's age.
	const asserted = `coalesce(
		nullif(frontmatter->>'last_explicit_reinforce', ''),
		to_char(created, 'YYYY-MM-DD HH24:MI:SS'))`
	rows, err := s.pool.Query(ctx, `
		select path, project,
		       coalesce(confidence, 0),
		       coalesce(maturity, ''),
		       coalesce(frontmatter->>'last_reinforced', ''),
		       `+asserted+`
		from notes
		where path like 'notes/%'
		  and status not in ('falsified', 'paused')
		  and not (frontmatter ? 'graduated_at')
		  and ($2 = '' or project = $2)
		  and `+asserted+` <= $1
		order by `+asserted,
		cutoff.Format("2006-01-02 15:04:05"), project)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var out []DecayingNote
	for rows.Next() {
		var d DecayingNote
		if err := rows.Scan(&d.Path, &d.Project, &d.Confidence, &d.Maturity,
			&d.LastReinforced, &d.LastAsserted); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// Referrer is a note that mentions some basename, with everything needed to
// recompute its outbound links — no disk read required.
type Referrer struct {
	ID          uuid.UUID
	Path        string
	Frontmatter map[string]any
	Body        string
}

// ReferrersOf returns notes whose body or `sources:` mention any of the given
// wikilink basenames.
//
// This is the reverse lookup link resolution needs and never had. resolveLinks
// drops a target it cannot resolve, so a note referencing one that does not
// exist yet loses that edge permanently: the referrer is unchanged, so drift
// detection skips it forever, and nothing re-resolves it when the target
// finally lands. Both halves of a reference live in columns already — body
// wikilinks in `content`, provenance in `frontmatter->'sources'` — so this
// needs no schema change and no file read.
//
// Body matching is a substring test on the literal `[[name]]` form. That can
// over-match (a name inside a code fence still counts), which is harmless:
// the caller re-resolves through vault.Targets, and a note that turns out not
// to reference the target simply rewrites the same edges it already had.
func (s *Store) ReferrersOf(ctx context.Context, names []string) ([]Referrer, error) {
	const op = "store.ReferrersOf"
	if len(names) == 0 {
		return nil, nil
	}
	wiki := make([]string, 0, len(names))
	srcs := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		wiki = append(wiki, "%[["+n+"]]%")
		srcs = append(srcs, "[["+n+"]]")
	}
	if len(wiki) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		select id, path, frontmatter, content
		from notes
		where content like any($1)
		   or exists (
		        select 1 from jsonb_array_elements_text(
		          case when jsonb_typeof(frontmatter->'sources') = 'array'
		               then frontmatter->'sources' else '[]'::jsonb end) s
		        where s = any($2))
		order by path`, wiki, srcs)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var out []Referrer
	for rows.Next() {
		var r Referrer
		var fm []byte
		if err := rows.Scan(&r.ID, &r.Path, &fm, &r.Body); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		if err := json.Unmarshal(fm, &r.Frontmatter); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// DeleteNote removes a note (chunks/links cascade). Deleting a missing path
// is not an error — deletion is idempotent for the watcher's sake.
func (s *Store) DeleteNote(ctx context.Context, path string) error {
	if _, err := s.pool.Exec(ctx, `delete from notes where path = $1`, path); err != nil {
		return cogerr.E("store.DeleteNote", cogerr.Internal, err)
	}
	return nil
}

// ReplaceChunks swaps a note's chunk set wholesale (the row shape carries
// content_hash so delta re-embedding stays possible).
func (s *Store) ReplaceChunks(ctx context.Context, path string, chunks []Chunk) error {
	const op = "store.ReplaceChunks"
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `delete from chunks where note_path = $1`, path); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	for _, c := range chunks {
		if _, err := tx.Exec(ctx, `
			insert into chunks (note_path, ordinal, heading_path, content, content_hash)
			values ($1,$2,$3,$4,$5)`,
			path, c.Ordinal, c.HeadingPath, c.Content, c.ContentHash); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// SetLinks replaces a note's outbound edges. Dangling targets are the
// caller's problem to resolve (or drop) before calling.
func (s *Store) SetLinks(ctx context.Context, src uuid.UUID, links []Link) error {
	const op = "store.SetLinks"
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Unavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `delete from links where src_note_id = $1`, src); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	for _, l := range links {
		if _, err := tx.Exec(ctx, `
			insert into links (src_note_id, dst_note_id, kind) values ($1,$2,$3)
			on conflict do nothing`, src, l.Dst, l.Kind); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// FileState is the reconciliation fast-path snapshot: the last
// known-good mtime/size/hash per path.
type FileState struct {
	Mtime  time.Time
	Size   int64
	Blake3 string
}

// FileStates returns the snapshot for every indexed note.
func (s *Store) FileStates(ctx context.Context) (map[string]FileState, error) {
	const op = "store.FileStates"
	rows, err := s.pool.Query(ctx, `select path, mtime, size, blake3_hash from notes`)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	out := map[string]FileState{}
	for rows.Next() {
		var p string
		var fs FileState
		if err := rows.Scan(&p, &fs.Mtime, &fs.Size, &fs.Blake3); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out[p] = fs
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

// CountChunks / CountLinks back tests proving derived tables cascade and
// rebuild; cheap enough to keep as public introspection.
func (s *Store) CountChunks(ctx context.Context, path string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from chunks where note_path = $1`, path).Scan(&n)
	if err != nil {
		return 0, cogerr.E("store.CountChunks", cogerr.Internal, err)
	}
	return n, nil
}

// LinkDsts returns the destination ids of a note's outbound edges —
// introspection for tests and link-graph tooling.
func (s *Store) LinkDsts(ctx context.Context, src uuid.UUID) ([]uuid.UUID, error) {
	const op = "store.LinkDsts"
	rows, err := s.pool.Query(ctx, `select dst_note_id from links where src_note_id = $1`, src)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var d uuid.UUID
		if err := rows.Scan(&d); err != nil {
			return nil, cogerr.E(op, cogerr.Internal, err)
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		return nil, cogerr.E(op, cogerr.Internal, rows.Err())
	}
	return out, nil
}

func (s *Store) CountLinks(ctx context.Context, src uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from links where src_note_id = $1`, src).Scan(&n)
	if err != nil {
		return 0, cogerr.E("store.CountLinks", cogerr.Internal, err)
	}
	return n, nil
}
