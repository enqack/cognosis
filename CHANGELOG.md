# Changelog

All notable changes to Cognosis are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Breaking

- **Token names are now unique among *live* tokens only, and token ids are UUIDv7.** Both change the
  schema and the token format, so this **requires a full rebuild**: stop the daemon,
  `drop schema public cascade; create schema public;`, restart. The vault is the source of truth and
  the derived index rebuilds from it, but **the `tokens` table is destroyed** — every token must be
  re-minted and every client re-pointed at its new token file. `parseToken` rejects UUIDv4 ids, so
  tokens minted before this release cannot authenticate even if their rows survived.

  Rotation is the payoff: `revoke <name>` then `create <name>` reuses the name, instead of burning it
  and forcing `desktop-2`, `desktop-3`. The daemon keeps the plain name `local` across rotations, so
  `token=local` in a log line always means the daemon itself.

### Added

- **The keyword leg falls back to OR when its conjunction comes back near-empty.**
  `websearch_to_tsquery` ANDs its terms and chunking is per-heading, so a query whose terms are
  spread across a note's sections matched none of its chunks and the note was absent from results
  entirely — not merely ranked low. When the AND leg returns fewer than 2 candidates it now re-runs
  with OR and keeps that result only if it found more. Threshold 2 rather than 1 because firing only
  on an empty result is measurably insufficient: the query that motivated this returned exactly one
  candidate, belonging to the wrong note, and `fallback@1` is identical to shipped behaviour there.
  On the 8000-chunk evaluation corpus it lifts target-note recall on those queries from 0.067 to
  0.400 while firing on zero of 30 healthy queries. `LegStats.FTSFallback` reports when it fires and
  is logged per query, because a silent fallback is indistinguishable from a healthy keyword leg in
  the counts.
- **`cognosis token prune`** deletes revoked tokens that nothing in `audit_log` references, with
  `--dry-run` sharing the delete's predicate so a preview cannot drift from the action. Referenced
  tokens are kept by design — the audit trail joins to them — so a revoked token surviving a prune
  means it was used. Clears the `ci-revoke-*` rows `scripts/checks/platform.sh` leaks per run.
- **`cognosis token create` validates names** (`a-z0-9_-`, 1–32 characters) and rejects `local`.
  Previously it validated nothing, so `token create local` succeeded and pushed the daemon onto a
  fallback name, after which the documented remedy `cognosis token revoke local` revoked the
  operator's token rather than the daemon's.
- `cognosis token list` shows an id prefix, which disambiguates the several revoked rows a rotated
  client leaves behind under one name.
- `store.RankFTSMode` exposes the tsquery connective; `store.RankFTS` keeps its signature and
  semantics.
- `internal/query/retrievaleval` can generate queries that starve the keyword leg — markers unique
  per chunk within a note, optionally with a decoy chunk elsewhere carrying the whole conjunction so
  AND returns exactly one wrong candidate. `TestKeywordORFallbackSweep` sweeps the threshold over
  both regimes plus a healthy control, and is wired into `scripts/checks/retrieval-eval.sh`.

- **MCP-originated log lines carry `token=<name>`.** `auth.NewIdentityHandler` wraps the daemon's slog
  handler and annotates any record logged with a context carrying an authenticated identity. This makes
  per-leg retrieval telemetry separable by client: `query_knowledge`'s `vector`/`fts`/`graph`/`sources`/
  `fts_fallback` counters live only in the log, never in `audit_log`, so before this there was no way to
  tell one caller's query shape from another's — an agent investigating retrieval produced traffic
  indistinguishable from ordinary use. A missing `token=` marks daemon-internal work (watcher, migration
  worker, CLI-driven compiles), not a gap. Requires per-client tokens to be useful; see remote.md.
  `audit_log` is unchanged and still keys on `token_id`, which joins to `tokens.name` at read time.

### Changed

- **`EnsureLocalToken` no longer mints under a `local-<8hex>` fallback.** A live `local` row with no
  state-dir file is operator error — a fresh state dir pointed at an existing database, or a
  mis-ordered rotation — and the daemon now refuses to start, naming the remedy, rather than running
  under a name nothing recognises. Repair of a genuinely stale file (the post-rebuild case the
  function exists for) is unchanged.
- Request-scoped log calls in `internal/mcpserver` now use slog's `*Context` variants. A plain `Info()`
  there silently drops the caller's identity, so it is enforced two ways: a test that parses the package
  and fails naming `file:line`, and `sloglint`'s `context` check scoped to that package alone.
- The golden fused ranking moved: `notes/scoped.md` now places above `entries/vault.md` for the
  fixture query. The fallback fires on that corpus and surfaces a note containing one query term
  above one containing none, which is the intended effect. `TestTuningFTSFallbackReachesRun` pins
  both orderings.
- `evalSpec` scales `Clusters` down when notes are thin (never up, so the default sweeps are
  unaffected). With 40 clusters over 25 notes most generated queries asked for a cluster no note
  belonged to, which read as keyword-leg starvation and depressed relevance — a fixture artifact
  that produced a false collateral-damage reading for the fallback.

## [0.2.0] - 2026-07-19

A correctness release, and a deliberately breaking one. Cognosis is pre-1.0, so breaking changes
take a MINOR bump rather than a migration; three land here.

**BLAKE3 replaces the last sha256 call sites.** Stored chunk hashes keep their old values until each
note is next edited — nothing reads that column, so it is inert. The derived Postgres socket
directory also moves, and that one is not inert: **stop the daemon before upgrading.** An old and a
new binary can otherwise place the socket in different directories, reach different postmasters, and
both run at once, because the single-instance advisory lock only excludes daemons that reached the
same database. This applies only when no `dsn` is configured and the daemon self-locates a dev
Postgres whose `.pg-data` path exceeds 90 characters; a configured DSN never reaches it.

**`cognosis vault restore` now routes through the daemon** when one owns the vault, so a restore
takes the same per-path lock as every other writer. It refuses rather than falling back if the
daemon owns the vault but cannot be reached — `--force-local` is the documented way through.

**`cognosis note delete --hard` is refused while a daemon owns the vault.** There is no MCP
equivalent to route it through, so stop the daemon first.

One further upgrade action, for vaults created before this release: generated and editor state is no
longer committed, but already-tracked copies stay tracked and will keep the vault repo dirty — which
also blocks `note delete --hard`. The one-time `git rm --cached` is in
[the setup guide](docs/setup-guide.md).

### Added

- **`cognosis status` gained `auth` and `graph` checks.** The existing five checks answer "the
  process is up and its dependencies respond", which is not the same claim as "the thing works":
  every expensive failure found while dogfooding was invisible to them. `auth` verifies the stashed
  local token actually authenticates, through the same code path provisioning uses rather than a
  second copy. `graph` re-derives every note's outbound links from indexed content and diffs them
  against the stored edges — the one form of index corruption nothing else notices, since notes,
  chunks and embeddings all stay correct while edges go missing. Both were verified by reproducing
  the original failures on a live vault.
- **`trust_local_errors` config key** (default `false`). Releases the full cause of `internal` and
  `unavailable` tool failures to a local caller, instead of the redacted summary. It is one of three
  conditions that must all hold: the operator sets the key, `bind_address` is loopback, and the
  individual call carries no proxy-forwarding header. The key is an operator assertion rather than a
  detection — under the reverse-proxy topology `docs/remote.md` recommends, the proxy forwards from
  `127.0.0.1` and every remote caller looks local, so network position alone cannot answer the
  question. The header check is evaluated per request, since a daemon can be bound to loopback and
  still be fronted by a proxy on the same host; it only ever removes trust, so a forged marker
  yields less detail, never more. A call arriving without HTTP header metadata withholds.
- **`edit_note` MCP tool** — change part of an existing note without resending the whole file. It
  replaces one exact, unique occurrence and then runs the same pipeline as `write_note`:
  revalidation, history commit, re-chunk, re-embed, re-index, referrer repair. An `old_string`
  matching nothing or matching several times is refused with the count, because the caller cannot
  see the file and "first match" would be a guess about content they are not looking at. The read
  and the write share one per-path lock, so two concurrent edits cannot silently lose one.

### Changed

- **BLAKE3 is now the project's only hashing primitive.** The last three sha256 call sites moved
  over: chunk content hashes, the derived fallback Postgres socket directory, and the deterministic
  stub embedding seed. Every content hash that mattered already used BLAKE3, so this leaves one
  answer to "did this content change" rather than two. Taken as a breaking change rather than a
  migration, per the pre-1.0 posture. Three consequences worth knowing:
  - **Stored chunk hashes do not heal on restart.** Reconciliation decides what to re-index from the
    *file*-level digest, which did not change, so existing rows keep their sha256-era values until
    each note is next edited. Nothing reads the column — it is written at three sites and never
    compared — so this is inert; a vault wanting uniform values must be rebuilt, not restarted.
  - **The derived dev socket directory moved**, which can defeat the single-daemon invariant on that
    one path. `store.sockDir` hashes the repo path to place a Unix socket when `.pg-data` is too
    long for `sun_path`, so an old and a new binary can locate *different postmasters* — and the
    instance lock only excludes daemons that reached the same database. It is narrow: only the
    dev self-locate path (no `dsn` configured), only when `.pg-data` exceeds 90 characters, and only
    before `.pg-socket-path` has been written, since that file is consulted first. A configured DSN
    never reaches it. Where it does apply, **stop the daemon before upgrading.** The dev shell now
    derives the same directory with `b3sum`, so the shell and `store.sockDir` cannot drift apart
    again.
  - Retrieval goldens are unchanged and were not regenerated: the fixture pins its vectors
    explicitly and never reaches the hash path.
- **Tool results no longer carry the internal op and error kind.** `write.Pipeline.Edit: validation:
  old_string appears 2 times` made an agent parse past two internal identifiers to reach the
  sentence it could act on. Those identifiers are not lost — the audit log and the structured log
  still record the full error — they are just off the surface where they were noise.
- **`Internal` and `Unavailable` causes are withheld from tool results**, replaced with a message
  naming the class and pointing at the daemon log. Both wrap raw pgx, os and net errors that carry
  DSNs, unix socket paths, database users and embedding endpoints, none of which an agent can act
  on and all of which a tool result would carry furthest. The two keep distinct messages, so "retry
  later" stays distinguishable from "report a bug".
- **Changing an existing note's id is refused** (`Conflict`). The index treats
  same-path-different-id as an eviction, dropping the row and re-pointing its inbound links, so a
  note's identity would churn on any write that supplied a fresh id. Omit the id to keep the
  existing one.
- **A blank `id:` is treated as absent** rather than producing a duplicate key. Previously it was
  rejected with `mapping key "id" already defined at line 1`, naming a line the caller never wrote.
- **`compile_lifecycle` reports a `skipped` action** for a note whose file changed while the run was
  in flight. The run walks the vault once and rewrites much later, so it could otherwise overwrite
  an edit that landed in between; the lifecycle is idempotent, so a skipped note is simply
  re-evaluated on the next compile.
- **`write_note` assigns a note id when frontmatter omits one** — a new UUIDv7 for a new path, and
  the *existing* id when overwriting. The contract requires v7 and the MCP surface previously
  offered no way to mint one, so every note written through it needed an out-of-band generator.
  Reuse on overwrite is load-bearing: `UpsertNote` evicts on same-path-different-id, so minting
  unconditionally would churn a note's identity on every update. A supplied id is still honoured
  and still rejected if it is not v7.
- **`write_note`'s description names the constraints previously learned only by rejection** — the
  legal `category` values per stage, and that notes under `notes/` require non-empty `sources`, so
  the entry has to be written first.
- **`cognosis vault restore` routes through the running daemon** when one owns the vault, and
  refuses rather than falling back when a daemon is present but unreachable — a direct write there
  is the race. `--force-local` overrides, warning about what it bypasses.
- **`cognosis note delete --hard` refuses while a daemon owns the database.** It writes Postgres
  rows, removes the file, rewrites `log.md` and purges git history, none of it under the shared
  lock, and there is no MCP tool to route it through. `token` and `embeddings` are deliberately
  unaffected: their DB-direct writes are a coordination medium a running daemon polls.
- **`query_knowledge` logs per-leg candidate counts** (`vector`, `fts`, `graph`, `fused`). The fused
  result count cannot say whether the keyword leg contributed anything, and on real traffic it
  frequently contributes nothing. Counts only, never query text.

### Fixed

- **The `graph` status check no longer fails on large vaults.** It resolved link targets once per
  note against a full table scan, so the audit was quadratic and exceeded its own deadline — a
  healthy daemon reported `graph FAIL` because its vault had grown. Now three queries regardless of
  size (measured at 2000 notes: 1.92s to 5.2ms).
- **A concurrent `compile_lifecycle` and `edit_note` can no longer revert each other.** They write
  the same vault files through different code paths and serialized only against themselves.
- **`cognosis status` cannot hang.** Its health-check connection was the one probe without a
  timeout, so an unreachable-but-not-refusing database would block the command indefinitely.
- **`cognosis note delete --hard` no longer half-completes.** The history rewrite ran last, after
  the index row, the file and `log.md` were already gone — and it is the step that fails, because
  `git filter-branch` refuses a dirty working tree and the vault is dirty whenever an editor or the
  daemon is writing. A failure left the note erased from the vault but still present in history, a
  state no retry could reach. The rewrite now runs first, so a failure destroys nothing; pending
  drift is committed before it; and the removal is committed after, instead of being left for
  whatever happened to commit next.
- **Vault history no longer records editor state or the generated dashboard.** `CommitAll` staged
  everything in the vault directory, so anything another tool wrote became part of the knowledge
  audit trail — 22% of a real vault's commits touched no note at all, and some carried
  `watch: <note>.md edited out-of-band` subjects while containing only editor churn. It now stages
  the four stage directories, `log.md` and `index.md`. Existing vaults still *track* those files;
  see the setup guide for the one-time `git rm --cached`.
- **A note's commit no longer sweeps in unrelated staged files, or swallows a concurrent write.**
  Only the `git add` was scoped; the `git commit` was not, and git commits whatever is in the index —
  so anything a human had staged in the vault repo rode along under a message naming only the note
  Cognosis had just written. The commit is now assembled in a scratch index, which also fixes a
  second problem the obvious scoped form (`git commit -- <paths>`) would have introduced: that is a
  *partial commit* and reads the working tree, so a note written by another goroutine between the
  stage and the commit was absorbed into the wrong message and then found nothing left to record —
  losing that version from vault history with no error anywhere. The commit is now a snapshot taken
  at stage time, and another party's staged work is neither committed nor discarded.
- **`index.md` is now versioned.** It carries the vault's `okf_version` declaration — the one field
  that says how everything else should be read — and Cognosis validates it but never writes it, so
  it was being skipped as though generated. A format declaration could change or vanish with nothing
  in the history saying so.
- **The CLI no longer briefly becomes the daemon it is checking for.** The daemon-ownership probe
  behind `vault restore` routing and the hard-delete guard acquired the single-instance advisory
  lock and released it. For the moment it held that lock it *was* the arbiter, so a daemon starting
  in that window exited with "another cognosis daemon already owns this database", naming a CLI
  process that had already finished. The probe now reads `pg_locks` instead, which cannot displace
  anyone.
- **`cognosis vault restore` no longer races the daemon.** It wrote the vault file and committed
  directly, taking neither the per-path lock the daemon's own writers share nor the path
  normalisation the equivalent MCP tool applies, so a restore concurrent with a compile pass over
  the same note interleaved freely.
- **CLI commands no longer migrate the schema underneath a running daemon.** `withStore` ran
  `store.Migrate` unconditionally, so any store-using command — including read-only ones like
  `token list` — could apply a migration to a database a live daemon was serving from.

## [0.1.2] - 2026-07-16

Maintenance release: no new memory features, but a pass over how an agent discovers the memory that
already exists — the injected session context now says what the vault is for, and four tool descriptions
say when to reach for them — plus end-to-end TLS coverage, a check suite that reports skips honestly
instead of aborting on them, and four agent definitions for review, tests, docs, and release.

### Changed

- **The injected session context now says what the vault is for** — `cognosis context inject` led with a
  bare list of note paths, which an agent could read in full and still not learn that the vault is its
  own memory rather than project files to browse. It now opens with a short preamble naming the tools
  and when to reach for them. This ships every session by design: sessions start cold, so there is no
  once-per-repo place to put it. The preamble is fixed overhead exempt from `--budget`, which still
  governs the index alone — so a small budget drops note lines rather than the framing. Two tests guard
  it — one that the preamble only ever names tools the server actually registers (asked over an
  in-memory transport, not a hand-copied list), and one that no description regresses to a bare
  mechanism blurb.
- **Four MCP tool descriptions say when to use the tool, not just what it does** — `write_note`,
  `query_knowledge`, `list_notes`, and `get_note` were mechanism-first, describing their own internals
  to an agent that needed to know when to reach for them. Each now leads with the occasion and
  disambiguates its nearest neighbour, since the real failure mode was `list_notes` vs `query_knowledge`
  and `get_note` vs retrieval rather than vagueness. The other eight already stated when and why and are
  unchanged.
- **TLS is now proven end-to-end** — a new `scripts/checks/tls.sh` covers the half of the built-in-TLS
  feature nothing exercised: that a non-loopback bind really is accepted once `tls.cert_file`/
  `tls.key_file` are set (the mirror of `daemon.sh`'s refusal case), that the handshake completes against
  a real CA, that plaintext is refused on the TLS port, and that bearer auth still gates the encrypted
  door. Its throwaway trust chain is generated by a new `gen-cert` mode in the check harness — in Go
  rather than openssl, which the dev shell does not provide.
- **A check with missing prerequisites now skips instead of failing the run** — `_lib.sh` has always
  exited 2 to mean "skip", but `check-all.sh` ran under `set -e` and treated it as a failure, so a
  machine without Ollama aborted at the second check and never ran the remaining four. Skips are now
  reported and the run continues; a genuine failure still stops it at the first one, and a run where
  everything skipped exits 2 rather than claiming success having verified nothing. Relatedly, the check
  harness no longer exits 2 for a usage error — now that 2 carries meaning, a programmer mistake must not
  be able to masquerade as a missing prerequisite.
- **`boot_daemon` honors an explicit bind** — it now derives `PORT` from a bind passed by the caller
  instead of overwriting it with a random one, so a check can reach a daemon that binds somewhere other
  than loopback.
- **The `-short` guard is now reachable through mage** — `mage testShort` runs the suite with `-short`,
  skipping the 5k-chunk migration load test, for a fast inner loop. `mage test` is unchanged and still
  proves the load claim on every CI push; until now the `testing.Short()` guard could not be reached
  through mage at all.
- **`internal/cogerr` now covers its own contract** — the package every other test asserts *through*
  (`cogerr.Is`) covers the Kind strings the MCP layer maps to tool errors, the unknown-Kind default,
  the nil-cause signalling form, and Kind survival through a `%w` wrap.
- **Four agent definitions now live under `.claude/agents/`** — `code-reviewer`, `test-author`,
  `docs-sync`, and `release-readiness`, each scoped to what the linter and a diff cannot see.
  `code-reviewer` targets the load-bearing invariants whose violation still passes the suite
  (vault-as-source-of-truth, RRF determinism, the single-instance lock's dedicated connection,
  soft-delete hygiene, migration read-availability, auth/audit). `test-author` carries the conventions
  invisible from a diff: `storetest`'s per-test schema, `embedtest`'s pinned-vector geometry,
  `COGNOSIS_TEST_DSN` vs `COGNOSIS_DSN`, the absence of `t.Parallel`, and goldens with no `-update` flag.
  `docs-sync` covers the sync set nothing in CI enforces — a new MCP tool touches the hardcoded tool
  count, README's literal name list, and `session-end-nudge.sh`'s allowlist; a new config key without its
  `SetDefault` silently breaks `config get`/`set`. `release-readiness` is a read-only go/no-go whose
  headline check is that `getVersion` consults `GITHUB_REF_NAME` before the `VERSION` file with nothing
  comparing them, so a disagreeing tag wins silently in published artifacts.

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

[unreleased]: https://github.com/enqack/cognosis/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/enqack/cognosis/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/enqack/cognosis/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/enqack/cognosis/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/enqack/cognosis/releases/tag/v0.1.0
