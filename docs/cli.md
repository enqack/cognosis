# Cognosis — CLI & MCP Tool Reference

Two surfaces: the `cognosis` command-line tool (operator/human) and the MCP tools (agent). The split
mirrors the safety boundary — mutating admin actions are CLI-driven; agents write and query through MCP.

## CLI

Run `cognosis <command> --help` for full flag detail. Commands are grouped by resource noun; daemon
lifecycle stays top-level.

### Daemon lifecycle
| Command | Purpose |
|---|---|
| `cognosis start [--foreground]` | Start the daemon (self-daemonizes; `--foreground` for a supervisor like systemd/launchd). Fails fatally if Postgres/embedding are unreachable. |
| `cognosis stop` | Stop the running daemon. |
| `cognosis status` | Health of postgres / embedding / schema / mcp / daemon. |
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
| `cognosis vault restore <path> --at <ref>` | Restore a note to a prior commit (itself a new commit; the running daemon reindexes it). |

### Notes (hard delete)
| Command | Purpose |
|---|---|
| `cognosis note delete <path> --hard [--yes]` | Genuine erasure — cascades across the file, index, embeddings, `log.md` mentions, and vault history. `--hard` is required (soft delete/archival is an MCP concern); `--yes` skips the interactive confirm. |

### Tokens
| Command | Purpose |
|---|---|
| `cognosis token create <name>` | Mint a bearer token; the plaintext is printed once and never stored. |
| `cognosis token revoke <name>` | Revoke a token (effective on the very next request). |
| `cognosis token list` | List token names and status. |

### Config
| Command | Purpose |
|---|---|
| `cognosis config get <key>` | Effective value (env > file > default). |
| `cognosis config set <key> <value>` | Persist a key to `config.yaml`. |

### Hook / session helpers
| Command | Purpose |
|---|---|
| `cognosis context inject [--project <p>] [--budget N]` | Emit a project-scoped knowledge index for a SessionStart hook. Marker-gated (no `.cognosis-project` → silent exit 0). `--budget` defaults to 2000. |
| `cognosis hook post-commit` | Capture the latest commit as a vault entry (called from a repo's `.git/hooks/post-commit`; marker-gated). |

---

## MCP tools

Exposed over Streamable HTTP after registering the server with a bearer token (see
[setup-guide.md](setup-guide.md)). Twelve tools:

### Write
- **`write_note(path, content, project?)`** — validate frontmatter, chunk, embed, and index a note
  atomically. `path` is vault-relative under `entries/`, `notes/`, `reflections/`, or `archive/`. An
  optional frontmatter `summary:` is cached and returned with hits.
- **`write_reflection(persona, description, content, project?, summary?)`** — write a persona-authored
  note into `reflections/`. Only the dry `description` is embedded; the styled `content` body is never
  indexed. `persona` must be enabled.

### Query & read
- **`query_knowledge(text, project?, top_k?, include_falsified?, include_archived?, persona_filter?, as_of?)`**
  — hybrid RRF retrieval (vector + keyword + graph). `top_k` default 8. `include_archived` surfaces
  soft-deleted notes; `include_falsified` surfaces retained-but-invalidated ones; `persona_filter`
  reweights by a persona's category bias; `as_of` (`YYYY-MM-DD HH:MM:SS`) answers "what did the KB
  believe at time T".
- **`get_note(path)`** — full raw content (frontmatter + body) for one note.
- **`list_notes(project?)`** — browse notes (path, category, status, project, updated, summary) without
  content.
- **`list_decaying(threshold_days, project?)`** — notes whose last reinforcement is at least
  `threshold_days` old — the reinforce shortlist (shielded notes excluded).

### Lifecycle
- **`compile_lifecycle(reinforce?, falsify?, dispute?, supersede?, graduate?, verify?, dry_run?)`** —
  the explicit, agent-justified pass. `reinforce`/`graduate` take id/path lists; `falsify`/`dispute`
  take id/path → reason maps; `supersede` maps a falsified note to its replacement; `verify` adds
  advisory related-context before terminal moves; `dry_run` writes nothing.

### Personas
- **`list_personas()`** — lightweight metadata for enabled personas (id, name, description).
- **`get_persona(id)`** — the full voice/structure/checklist for one persona.

### Vault history
- **`vault_history(path?, limit?)`** — read the commit log (whole-vault, or scoped to `path`); each
  entry carries the hash to feed `restore_note`. `limit` default 10.
- **`restore_note(path, ref)`** — restore a note to a prior state (`ref` from `vault_history`); a new
  commit, reindexed live.

### Migration status
- **`get_migration_status()`** — progress of the current embedding-provider migration (from/to,
  chunks done/total split between back-fill and lazy, paused state, last error), or idle.
