# Changelog

All notable changes to Cognosis are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0]

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

[0.1.0]: https://github.com/enqack/cognosis/releases/tag/v0.1.0
