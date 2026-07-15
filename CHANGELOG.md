# Changelog

All notable changes to Cognosis are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.1] - 2026-07-15

Maintenance and hardening release: no new memory features, but a security-focused pass (toolchain,
permissions, traversal), a panic-recovery safety net, a much wider static-analysis suite, and a fix
that gives the SessionEnd nudge real session context.

### Changed

- **Static analysis expanded 5 → 26 linters + gosec** — enabled bodyclose, errorlint, noctx,
  sqlclosecheck, gocritic, unparam, and more, with documented gosec/contextcheck carve-outs; every
  resulting finding was fixed in code (context threaded through subprocess/dial/exec calls, error
  wrapping, dead-store and duplicate-scan cleanups) rather than silenced.
- **Dev environment** — the Nix dev shell now provides `jq` (used by the SessionEnd hook), and the flake
  pre-creates the pgvector extension in the `public` schema to avoid a parallel-test `CREATE EXTENSION`
  race.
- **Docs & housekeeping** — setup-guide, architecture, and the magefile/lint comments were refreshed to
  match the toolchain, linter policy, and panic-recovery behavior; `.gitignore` now covers `dist/` and
  `.idea/`.

### Removed

- A 16 MB `harness` binary that had been committed to the repo root.

### Fixed

- **Panic-recovery net** — a panic in per-item background work (a reconcile-sweep file worker, a lazy
  embedding-migration batch) is now recovered and logged instead of crashing the daemon; a panic in a
  primary runner still fails the whole process, so there is still no silent degraded mode.
- **Surfaced previously-swallowed errors** — watching a newly-created vault subdirectory, the
  lazy-migration pre-check, recording a migration-worker error, closing the lifecycle audit log, an
  invalid lifecycle move destination, and non-string persona frontmatter fields now report or log their
  failures instead of dropping them.
- **SessionEnd nudge sees the session** — `hooks/session-end-nudge.sh` resumes the ending session (via
  the `session_id` on the hook's stdin) so the headless turn has real context instead of starting blank,
  and guards against a headless re-entry loop.
- **Harness version drift** — the end-to-end check harness (`scripts/checks/harness`) advertised a
  hardcoded `0.1.0` MCP client version; it now uses a stampable `version` var (default `dev`), mirroring
  `cmd/cognosis` so it can't drift from the release.

### Security

- **Go toolchain pinned to 1.26.5** — added `toolchain go1.26.5` to pick up a `crypto/tls`
  standard-library fix; builds on an older Go 1.25 transparently fetch it.
- **Tighter default permissions** — files the daemon creates drop from `0644` to `0600` and directories
  from `0755` to `0750` across the vault, lockfile, history repo, lifecycle audit log, and config paths.
- **Symlink-safe vault traversal** — reconciliation confines its walk with `os.Root`, closing the
  symlink-swap TOCTOU window a plain `WalkDir` + `ReadFile` pair leaves between the directory scan and
  the file open.
- **Auth & test hardening** — the Argon2 token comparison bounds its length conversion, and parallel-test
  schema names use `crypto/rand`.

## [0.1.0] - 2026-07-13

First release. A centralized, project-agnostic, long-term memory service for MCP-capable AI coding
agents: a background daemon that owns a markdown vault as the single source of truth, keeps a derived
Postgres index, and serves its memory over MCP (Streamable HTTP).

### Added

- **Memory loop** — `write_note` → `query_knowledge` (hybrid RRF over vector + keyword + link-graph
  legs, vector/keyword legs fanned out concurrently) → `get_note`, over Streamable HTTP MCP with local
  Ollama embeddings.
- **Knowledge lifecycle** — `compile_lifecycle`: explicit, agent-justified reinforce / falsify /
  dispute / supersede / graduate with automatic decay, archival, passive citation-refresh, in-place
  graduation, optional `verify` advisories, and `dry_run` previews; every run is one revertible history
  commit.
- **Temporal & visibility** — `as_of` temporal queries, `list_decaying`, and agent-supplied cached
  `summary` returned with retrieval hits.
- **Soft-delete hygiene** — archived/faded notes excluded from default retrieval (`include_archived`
  opts in), an `archived_at` stamp keeps `as_of` honest, and an RRF penalty on chunks linking to
  archived notes stops stale material from leaking back into context.
- **Vault history** — auto-managed local git recovery net, surfaced as a generated read-only
  `history.md` dashboard, the `vault_history` / `restore_note` MCP tools, and the
  `cognosis vault history` / `restore` CLI.
- **Single-instance invariant** — a Postgres session advisory lock (cross-machine arbiter) plus a local
  PID lockfile, with a liveness guard.
- **Cross-project links** — `[[project:basename]]` disambiguation in the link graph.
- **Personas** — two-tier discovery (`list_personas` / `get_persona`), `write_reflection` (only the dry
  description is embedded), and `persona_filter` as a retrieval lens. Deep Thoughts ships as the first
  persona.
- **Auth & audit** — per-client Argon2id bearer tokens checked synchronously (immediate revocation) with
  a debounced `last_used_at` touch, a zero-config local token, and a redacted audit trail.
- **Session integration** — `cognosis context inject` (marker-gated SessionStart), a SessionEnd nudge,
  opt-in git commit capture, and systemd/launchd service files.
- **Embedding migration** — zero-downtime provider/model switch (background back-fill + lazy
  touch-migration + dual-embed + query fan-out fallback), pausable/resumable/rollback-able, with
  `embeddings status` / `prune` and the `get_migration_status` MCP tool.
- **Remote access** — reverse-proxy or built-in TLS (the only door to a non-loopback bind); see
  `docs/remote.md`.
- **Tooling & docs** — Nix flake dev environment; Mage targets (`build`, `test`, `lint`, `check`,
  `install`, `release`); feature-scoped end-to-end checks under `scripts/checks/`; a single embedded
  schema migration; static analysis (golangci-lint) enforced by `mage lint` and CI; version stamping
  from `VERSION`; cross-compiled release archives with checksums; and the docs set under `docs/`.

### Deferred

- Slack/Discord bridge (explicitly post-v1).

[unreleased]: https://github.com/enqack/cognosis/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/enqack/cognosis/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/enqack/cognosis/releases/tag/v0.1.0
