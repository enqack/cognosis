-- Cognosis derived index — the complete v1 schema in one pass. notes.content
-- is a rebuildable mirror of the markdown vault; chunks/links stay fully
-- derived and droppable. Embedding tables are NOT here — they're provisioned
-- dynamically per provider (probe dimension -> CREATE TABLE at runtime).
create extension if not exists vector;

create table if not exists notes (
  path         text primary key,       -- vault-relative; mutable, cascades below
  id           uuid not null unique,   -- from frontmatter, never generated here
  project      text not null default '',
  category     text not null,          -- concept | cursed-knowledge | lesson-learned | reflection | entry
  status       text not null default 'active',
  confidence   double precision,       -- null outside decaying notes
  maturity     text,
  created      timestamptz not null,
  updated      timestamptz not null,
  frontmatter  jsonb not null,         -- full frontmatter, lossless
  content      text not null,          -- full note body (mirror)
  -- Cached one-line summary supplied by the writing agent (the writer IS the
  -- language model), returned with every retrieval hit as a cheap first-pass
  -- filter. The frontmatter `summary:` key is the source of truth; this column
  -- is derived like everything else in the index.
  summary      text not null default '',
  -- reconciliation state: last known-good file identity
  mtime        timestamptz not null,
  size         bigint not null,
  blake3_hash  text not null,
  indexed_at   timestamptz not null default now()
);

create table if not exists chunks (
  id           uuid primary key default gen_random_uuid(),
  note_path    text not null references notes(path) on delete cascade on update cascade,
  ordinal      int not null,           -- positional chunk identity within the note
  heading_path text,
  content      text not null,
  content_hash text not null,          -- chunk-level, enables delta re-embeds
  fts          tsvector generated always as (to_tsvector('english', content)) stored,
  unique (note_path, ordinal)
);

-- Resolved outbound links: body wikilinks and provenance. Edges reference the
-- stable note id (not path) so a move never orphans the graph.
create table if not exists links (
  src_note_id uuid not null references notes(id) on delete cascade,
  dst_note_id uuid not null references notes(id) on delete cascade,
  kind        text not null,           -- 'wikilink' | 'source'
  primary key (src_note_id, dst_note_id, kind)
);

-- Registry of provisioned embedding provider/model tables. The tables
-- themselves are created at runtime (probe dimension -> CREATE TABLE).
create table if not exists embedding_providers (
  name       text not null,
  model      text not null,
  dimension  int not null,
  table_name text not null unique,
  active     boolean not null default false,
  created_at timestamptz not null default now(),
  primary key (name, model)
);

-- Bearer-token auth + audit trail. Only Argon2id hashes are stored; the
-- plaintext token is printed once at creation and never again.
-- name carries no table-level UNIQUE: uniqueness is scoped to live tokens by
-- tokens_live_name_idx below, so a revoked row keeps its name for the audit
-- join without squatting it.
-- id carries no default on purpose: every creator passes an explicit UUIDv7
-- (embedded in the plaintext for O(1) verification lookup), and parseToken
-- rejects v4 — a gen_random_uuid() default would mint tokens that can never
-- authenticate.
create table if not exists tokens (
  id           uuid primary key,
  name         text not null,
  token_hash   text not null,
  created_at   timestamptz not null default now(),
  revoked_at   timestamptz,
  last_used_at timestamptz
);

-- Every tool call is audit-logged. args_summary is a redacted form (tool name
-- plus key identifying args like path or project) — never note content.
create table if not exists audit_log (
  id           bigint generated always as identity primary key,
  token_id     uuid references tokens(id),
  tool_name    text not null,
  project      text not null default '',
  args_summary text not null default '',
  ts           timestamptz not null default now(),
  success      boolean not null,
  error        text not null default ''
);

-- Embedding-provider migration state: the coordination medium between the CLI
-- (which starts/pauses/rolls back migrations) and the daemon's back-fill
-- worker (which does the work), and the source of the progress report.
-- chunks_backfill + chunks_lazy converging on chunks_total is the completion
-- invariant.
create table if not exists migration_state (
  id              uuid primary key default gen_random_uuid(),
  from_name       text not null,
  from_model      text not null,
  from_table      text not null,
  to_name         text not null,
  to_model        text not null,
  to_table        text not null,
  status          text not null default 'in_progress', -- in_progress | complete | rolled_back
  paused          boolean not null default false,
  chunks_total    int not null default 0,
  chunks_backfill int not null default 0,
  chunks_lazy     int not null default 0,
  chunks_failed   int not null default 0,
  started_at      timestamptz not null default now(),
  finished_at     timestamptz,
  last_error      text not null default ''
);

create index if not exists notes_project_idx on notes (project);
create index if not exists notes_id_idx on notes (id);
create index if not exists chunks_fts_idx on chunks using gin (fts);
create index if not exists links_dst_idx on links (dst_note_id);
create index if not exists audit_log_ts_idx on audit_log (ts);
create index if not exists audit_log_token_idx on audit_log (token_id);

-- One *live* token per name. Rotation is revoke-then-recreate under the same
-- name, so the daemon keeps the plain name `local` across rotations and clients
-- keep a stable `token=<name>` attribution attribute. Revoked rows stay for the
-- audit_log join, which keys on token_id and so is unaffected by reuse.
create unique index if not exists tokens_live_name_idx
  on tokens (name) where revoked_at is null;

-- At most one migration may be in progress.
create unique index if not exists migration_state_single_active
  on migration_state ((true)) where status = 'in_progress';
