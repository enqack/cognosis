# Cognosis

A centralized, project-agnostic, long-term memory service for MCP-capable AI
coding agents. Cognosis runs as a background daemon, owns a markdown vault as
the single source of truth, keeps a derived Postgres index (chunks, embeddings,
link graph), and exposes its memory over MCP (Streamable HTTP): write a note,
query it back hybrid-ranked (semantic + keyword + link graph, fused by
reciprocal rank), fetch it whole.

## Features

Everything below is implemented and proven by feature-scoped end-to-end checks
(`scripts/checks/*.sh`, run together via `scripts/check-all.sh` / `mage check`)
plus a `-race` test suite.

New here? See [docs/setup-guide.md](docs/setup-guide.md) for full setup.

- **Memory loop**: `write_note` → `query_knowledge` (semantic + keyword +
  link-graph, RRF-fused) → `get_note`, over Streamable HTTP MCP with local
  Ollama embeddings.
- **Knowledge lifecycle**: `compile_lifecycle` — explicit, agent-justified
  reinforce/falsify/dispute/graduate with automatic decay, archival, passive
  citation-refresh, and in-place graduation to canon; every run is one
  revertible history commit; `dry_run` previews without writing.
- **Temporal + visibility**: `query_knowledge` takes `as_of` ("what did the
  KB believe at time T" — existence and status at T; recover past *content*
  via `vault restore`), and `list_decaying` surfaces the reinforce shortlist.
- **Soft-delete hygiene**: archived/faded notes drop out of ordinary retrieval
  (`include_archived` surfaces them for audits), and any chunk whose note links
  to an archived note is severely penalized in RRF fusion — a dense stale
  reflection about a shelved concept can't leak back into the agent's context.
- **Vault history, surfaced**: a generated read-only `history.md` dashboard at
  the vault root lists the last revertable states with the exact restore
  command for each (refreshed every compile pass and on boot), and the
  `vault_history` / `restore_note` MCP tools let an agent read the commit log
  and mediate a rollback without the operator touching a terminal.
- **Single-instance invariant**: enforced by a Postgres **session advisory
  lock** held for the daemon's lifetime, not just a local PID lockfile — two
  daemons on different machines pointed at one database cannot both run (the
  lock releases automatically if a daemon crashes). The lockfile stays as a
  fast same-machine guard.
- **Cross-project links**: `[[project:basename]]` disambiguates colliding
  basenames across projects in the link graph.
- **Cached summaries**: an agent-supplied one-line frontmatter `summary:` is
  stored on the note row and returned with every retrieval hit and listing.
- **Retrieval-augmented compilation**: `compile_lifecycle` with `verify: true`
  surfaces the notes most related to each falsify/graduate target as advisory
  context before the move is final.
- **Personas**: two-tier discovery (`list_personas` metadata /
  `get_persona` full voice guide), `write_reflection` (only the dry
  description is embedded, never the styled body), and `persona_filter` as a
  retrieval lens. Deep Thoughts ships as the first inhabitant.
- **Auth + audit**: per-client bearer tokens (Argon2id at rest, checked
  synchronously — revocation is effective on the very next request), a
  zero-config auto-minted local token, and a redacted audit trail of every
  tool call.
- **Session hooks**: `cognosis context inject` (marker-gated: silent no-op
  outside `.cognosis-project` repos, loud 2s failure inside them), a
  SessionEnd nudge script, vault `history`/`restore`, hard delete with
  history purge, and systemd/launchd service files.
- **Embedding migration**: `cognosis embeddings migrate --from <n>/<m> --to <n>/<m>` switches
  providers/models with the system fully queryable throughout — a background back-fill worker,
  lazy touch-migration of queried chunks, dual-embedding of new writes, and the query fan-out as
  the fallback read. Pausable, resumable, rollback-able; the old table survives until an explicit
  `embeddings prune`. Progress via `embeddings status` or the `get_migration_status` MCP tool.
  Proven under load: a 5k-chunk corpus migrates while concurrent queries never once return empty.
- **Git commit capture** (opt-in): install `hooks/post-commit.sh` in a repo's
  `.git/hooks/` — commits in `.cognosis-project`-marked repos land as
  structured vault entries.
- **Remote access**: see `docs/remote.md` — reverse proxy terminating TLS
  (recommended; Cognosis stays on loopback), or built-in TLS via
  `tls.cert_file`/`tls.key_file` (the only door to a non-loopback bind).

## Quick start (dev)

```sh
nix develop        # Go toolchain, Postgres+pgvector, Ollama
pg-start           # dev Postgres on port 5434 (exports COGNOSIS_DSN)
ollama serve &     # or any Ollama already listening on :11434
ollama pull nomic-embed-text:v1.5
cognosis start     # fails fatally if Postgres/Ollama are unreachable
cognosis status    # daemon / postgres / schema / mcp / embedding health
```

The vault lives at `$XDG_DATA_HOME/cognosis/kb/`; config at
`$XDG_CONFIG_HOME/cognosis/config.yaml`. Hand-editing the vault (Obsidian,
any editor) is supported — the daemon reconciles out-of-band edits live, on
boot, and via a periodic sweep, and every change is versioned in an
auto-managed git history repo inside the vault.

## Using it from Claude Code

The daemon mints a local bearer token on first start
(`$XDG_STATE_HOME/cognosis/local-token`, mode 0600):

```sh
claude mcp add --transport http cognosis http://127.0.0.1:7433 \
  --header "Authorization: Bearer $(cat ~/.local/state/cognosis/local-token)"
```

That exposes `write_note`, `query_knowledge`, `list_notes`, `list_decaying`,
`get_note`, `compile_lifecycle`, `list_personas`, `get_persona`,
`write_reflection`, `vault_history`, `restore_note`, and
`get_migration_status` (see [docs/cli.md](docs/cli.md) for each tool's
arguments).
For automatic session context, wire up the hooks in `hooks/`
(`settings.sample.json`) — SessionStart injects a project-scoped index in
repos carrying a `.cognosis-project` marker; unmarked repos are untouched.
The server binds loopback only by default; a non-loopback bind requires
built-in TLS or a TLS-terminating reverse proxy — see
[docs/remote.md](docs/remote.md).

## Development

```sh
mage build   # -> bin/cognosis (version-stamped from VERSION)
mage test    # go test -race ./...
mage testShort # go test -race -short ./... (skips the 5k-chunk load test)
mage lint    # gofmt + golangci-lint (see .golangci.yml)
mage check   # end-to-end feature checks (scripts/check-all.sh)
mage release # cross-compiled, version-stamped archives + SHA256SUMS -> dist/
```

End-to-end checks live under `scripts/checks/`, organized by feature, and each
boots a daemon in a sandbox against the dev Postgres + Ollama:

```sh
./scripts/checks/daemon.sh              # startup gates, single-instance lock, shutdown, reconciliation
./scripts/checks/memory-loop.sh         # write -> hybrid query -> get -> list over MCP
./scripts/checks/retrieval.sh           # summaries, as_of, list_decaying, archived exclusion
./scripts/checks/knowledge.sh           # compile lifecycle + verify, personas, vault history
./scripts/checks/platform.sh            # auth/audit, context inject, commit capture, service files
./scripts/checks/tls.sh                 # built-in TLS: non-loopback bind, handshake, auth still enforced
./scripts/checks/embedding-migration.sh # zero-downtime provider migration, under load
./scripts/check-all.sh                  # all of the above, in order
```

Integration tests need `COGNOSIS_TEST_DSN` pointing at a Postgres with
pgvector (the dev shell provides one); Ollama-backed tests skip when no
server is reachable. The `scripts/checks/*.sh` end-to-end checks additionally
want `COGNOSIS_DSN` (e.g. `pg-start`) and a local Ollama with the pinned model —
a check whose prerequisites are missing reports itself skipped, and `check-all.sh`
carries on to the next one rather than failing the run.

## Documentation

- [docs/setup-guide.md](docs/setup-guide.md) — full system setup (Nix and manual/production), first
  start, Claude Code registration, hooks, service management, troubleshooting.
- [docs/architecture.md](docs/architecture.md) — how it's built and why (vault-as-source-of-truth,
  derived index, retrieval, lifecycle, history, single-instance lock).
- [docs/configuration.md](docs/configuration.md) — every `config.yaml` key, its default, and the
  `COGNOSIS_*` env override.
- [docs/cli.md](docs/cli.md) — the `cognosis` CLI and the MCP tool surface.
- [docs/remote.md](docs/remote.md) — remote access: reverse proxy or built-in TLS.
- [CHANGELOG.md](CHANGELOG.md) — release notes.
