# Cognosis -- CLI & MCP Tool Reference

Two surfaces: the `cognosis` command-line tool (operator/human) and the MCP tools (agent). The split
mirrors the safety boundary -- mutating admin actions are CLI-driven; agents write and query through MCP.

## CLI

Run `cognosis <command> --help` for full flag detail. Commands are grouped by resource noun; daemon
lifecycle stays top-level.

### Daemon lifecycle
| Command | Purpose |
|---|---|
| `cognosis start [--foreground]` | Start the daemon (self-daemonizes; `--foreground` for a supervisor like systemd/launchd). Fails fatally if Postgres/embedding are unreachable. |
| `cognosis stop` | Stop the running daemon. |
| `cognosis status` | Health of postgres / embedding / schema / mcp / daemon, plus `auth` (the stashed local token still authenticates) and `graph` (stored edges agree with what the indexed content implies). The last two exist because the first five can all be green while every client 401s or the link graph has silently lost edges. |
| `cognosis version` | Print the version (also `cognosis --version`). |

### Schema
| Command | Purpose |
|---|---|
| `cognosis schema status` | Applied schema version, dirtiness, and pending migrations. |

### Embeddings (provider migration)
| Command | Purpose |
|---|---|
| `cognosis embeddings migrate --from <name>/<model> --to <name>/<model> [--dry-run]` | Start a zero-downtime provider migration; `--dry-run` reports the plan and writes nothing. |
| `cognosis embeddings migrate --pause` / `--resume` / `--rollback` | Control an in-progress migration (rollback keeps the half-migrated table for a later attempt). |
| `cognosis embeddings status` | Progress/ETA for the current migration, or idle. |
| `cognosis embeddings prune <name>/<model>` | Drop a retired provider's table. Refuses the active provider or a party to an in-progress migration. |

### Vault history & recovery
| Command | Purpose |
|---|---|
| `cognosis vault history <path>` | Show a note's version history (newest first). |
| `cognosis vault restore <path> --at <ref>` | Restore a note to a prior commit (itself a new commit). Routes through the daemon when one owns the vault, so the restore takes the same per-path lock as every other writer; writes directly when none does. |
| `cognosis vault restore ... --force-local` | Write the vault directly even though a daemon owns it. Bypasses the per-path lock, so a concurrent compile or edit of the same note can be lost. Warns on use. |

### Notes (hard delete)
| Command | Purpose |
|---|---|
| `cognosis note delete <path> --hard [--yes]` | Genuine erasure -- cascades across the file, index, embeddings, `log.md` mentions, and vault history. `--hard` is required (soft delete/archival is an MCP concern); `--yes` skips the interactive confirm. **Refused while a daemon owns the vault** -- there is no MCP equivalent to route it through, so stop the daemon first. |

### Tokens
| Command | Purpose |
|---|---|
| `cognosis token create <name>` | Mint a bearer token; the plaintext is printed once and never stored. Names are `a-z0-9_-`, 1-32 chars; `local` is reserved for the daemon. |
| `cognosis token revoke <name>` | Revoke a token (effective on the very next request). Frees the name for reuse. |
| `cognosis token list` | List token names, id prefix and status. |
| `cognosis token prune [--dry-run]` | Delete revoked tokens no `audit_log` row references; referenced ones are kept by design. |

### Config
| Command | Purpose |
|---|---|
| `cognosis config get <key>` | Effective value (env > file > default). |
| `cognosis config set <key> <value>` | Persist a key to `config.yaml`. |

### Telemetry
| Command | Purpose |
|---|---|
| `cognosis telemetry query [--window N] [logfile ...]` | Per-leg retrieval counts (`vector`/`fts`/`graph`/`fused`) from the daemon log's `query_knowledge` events, as a CSV series with rolling fallback-firing and AND-starvation rates. Reads the live `daemon.log` by default; `-` reads stdin. Read-only -- retrieval tuning deliberately has no CLI surface. |

### Hook / session helpers
| Command | Purpose |
|---|---|
| `cognosis context inject [--project <p>] [--budget N]` | Emit the embedded agent SOP followed by a project-scoped knowledge index for a SessionStart hook. Marker-gated (no `.cognosis-project` -> silent exit 0). The SOP is fixed overhead; `--budget` (default 2000) governs the index alone. |
| `cognosis hook post-commit` | Capture the latest commit as a vault entry (called from a repo's `.git/hooks/post-commit`; marker-gated). |

---

## MCP tools

Exposed over Streamable HTTP after registering the server with a bearer token (see
[setup-guide.md](setup-guide.md)). Thirteen tools:

### Write
- **`write_note(path, content, project?)`** -- validate frontmatter, chunk, embed, and index a note
  atomically. `path` is vault-relative under `entries/`, `notes/`, `reflections/`, or `archive/`. An
  optional frontmatter `summary:` is cached and returned with hits.
- **`edit_note(path, old_string, new_string)`** -- change part of an existing note without resending the
  whole file, then revalidate, version, re-chunk, re-embed and re-index exactly as `write_note` does.
  `old_string` is matched literally and must appear **exactly once**: zero or several matches are
  refused with the count rather than guessed at, since the caller cannot see the file. Empty
  `new_string` deletes the matched text. The read and the write share one per-path lock, so
  concurrent edits cannot silently lose one.
- **`write_reflection(persona, description, content, project?, summary?)`** -- write a persona-authored
  note into `reflections/`. Only the dry `description` is embedded; the styled `content` body is never
  indexed. `persona` must be enabled.

### Query & read
- **`query_knowledge(text, project?, top_k?, include_falsified?, include_archived?, persona_filter?, as_of?)`**
  -- hybrid RRF retrieval (vector + keyword + graph). `top_k` default 8. `include_archived` surfaces
  soft-deleted notes; `include_falsified` surfaces retained-but-invalidated ones; `persona_filter`
  reweights by a persona's category bias; `as_of` (`YYYY-MM-DD HH:MM:SS`) answers "what did the KB
  believe at time T". A `project`-scoped query returns that project's notes plus untagged (global)
  ones; the same rule applies to `list_notes` and `list_decaying`.
- **`get_note(path)`** -- full raw content (frontmatter + body) for one note.
- **`list_notes(project?)`** -- browse notes (path, category, status, project, updated, summary) without
  content.
- **`list_decaying(threshold_days, project?)`** -- notes whose last reinforcement is at least
  `threshold_days` old -- the reinforce shortlist (shielded notes excluded).

### Lifecycle
- **`compile_lifecycle(reinforce?, falsify?, dispute?, supersede?, graduate?, verify?, dry_run?)`** --
  the explicit, agent-justified pass. `reinforce`/`graduate` take id/path lists; `falsify`/`dispute`
  take id/path -> reason maps; `supersede` maps a falsified note to its replacement; `verify` adds
  advisory related-context before terminal moves; `dry_run` writes nothing.

### Personas
- **`list_personas()`** -- lightweight metadata for enabled personas (id, name, description).
- **`get_persona(id)`** -- the full voice/structure/checklist for one persona.

### Vault history
- **`vault_history(path?, limit?)`** -- read the commit log (whole-vault, or scoped to `path`); each
  entry carries the hash to feed `restore_note`. `limit` default 10.
- **`restore_note(path, ref)`** -- restore a note to a prior state (`ref` from `vault_history`); a new
  commit, reindexed live.

### Migration status
- **`get_migration_status()`** -- progress of the current embedding-provider migration (from/to,
  chunks done/total split between back-fill and lazy, paused state, last error), or idle.
